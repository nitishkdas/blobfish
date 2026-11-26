package auth

import (
	"net/netip"
	"slices"
	"sync"
	"time"
)

// The sign-in throttle counts failed password guesses in fixed windows.
//
// An attempt is charged *before* its password is hashed (begin), and taken
// back afterwards if it was not a wrong guess (undo). Checking first and
// counting after the hash would let a burst of parallel requests all pass
// the check while they queue for a hashing slot.
//
// Counters live in two tables. Counters about real users ("pinned") are
// never dropped to make room: dropping them would let an attacker who fills
// the table wipe a victim's count and keep guessing. They stay bounded
// anyway: one per user, plus one per user and network, and a network gets
// a counter for a user only by making a guess that the per-user total
// allows. Counters about networks and names nobody has ("loose") can be
// created cheaply from many addresses, so that table has a cap and drops
// its oldest entries when full; losing one only resets a network's own
// count, never a user's.

const (
	// maxLooseEntries caps the loose table. Keys are short (names are
	// checked first), so this is well under 2 MB.
	maxLooseEntries = 10_000
	// prunePinnedAt: expired pinned counters are swept when the table
	// reaches this size (they are also replaced on their next use).
	prunePinnedAt = 10_000
	// knownNetsPerUser is how many networks a user is remembered to have
	// signed in from (see trusted).
	knownNetsPerUser = 8
	// knownFor is how long a successful sign-in vouches for a network.
	knownFor = SessionMax
)

// limit is one counter an attempt is charged to.
type limit struct {
	key string
	// max is the number of failures the window allows. 0 means count but
	// never refuse (see trusted).
	max int
	// pinned: a real user's counter, never dropped to make room.
	pinned bool
}

type failCount struct {
	n     int
	first time.Time
}

type throttle struct {
	mu     sync.Mutex
	pinned map[string]*failCount
	loose  map[string]*failCount
	// known maps a user to the networks they recently signed in from, and
	// when. Bounded by users × knownNetsPerUser; lost on restart, which
	// only makes the throttle stricter until the user signs in again.
	known map[string]map[string]time.Time
}

func (t *throttle) table(pinned bool) map[string]*failCount {
	if pinned {
		if t.pinned == nil {
			t.pinned = map[string]*failCount{}
		}
		return t.pinned
	}
	if t.loose == nil {
		t.loose = map[string]*failCount{}
	}
	return t.loose
}

// begin charges one attempt to every limit, all or nothing: if any limit
// is used up, nothing is counted and the wait until the last blocking
// window ends is returned. 0 means the attempt may go ahead.
func (t *throttle) begin(now time.Time, ls ...limit) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var wait time.Duration
	for _, l := range ls {
		f := t.table(l.pinned)[l.key]
		if l.max > 0 && f != nil && now.Sub(f.first) < throttleWindow && f.n >= l.max {
			wait = max(wait, f.first.Add(throttleWindow).Sub(now))
		}
	}
	if wait > 0 {
		return wait
	}
	for _, l := range ls {
		m := t.table(l.pinned)
		f := m[l.key]
		if f != nil && now.Sub(f.first) < throttleWindow {
			f.n++
			continue
		}
		if f == nil {
			t.makeRoom(m, l.pinned, now)
		}
		m[l.key] = &failCount{n: 1, first: now}
	}
	return 0
}

// undo takes back an attempt that begin charged at time at, because it
// turned out not to be a wrong guess (right password, or the client left
// before its password was checked). A window that started after at is
// someone else's count and is left alone.
func (t *throttle) undo(at time.Time, ls ...limit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range ls {
		m := t.table(l.pinned)
		if f := m[l.key]; f != nil && !f.first.After(at) {
			if f.n--; f.n <= 0 {
				delete(m, l.key)
			}
		}
	}
}

// reset forgets a counter.
func (t *throttle) reset(l limit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.table(l.pinned), l.key)
}

// makeRoom is called before adding a counter to m. Caller holds t.mu.
func (t *throttle) makeRoom(m map[string]*failCount, pinned bool, now time.Time) {
	switch {
	case pinned && len(m) >= prunePinnedAt:
		dropExpired(m, now)
	case !pinned && len(m) >= maxLooseEntries:
		dropExpired(m, now)
		if len(m) < maxLooseEntries*9/10 {
			return
		}
		// Still full: a flood of fresh networks or names. Drop the oldest
		// counters, down to 90%, rather than grow.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return m[a].first.Compare(m[b].first) })
		for _, k := range keys[:len(m)-maxLooseEntries*9/10] {
			delete(m, k)
		}
	}
}

func dropExpired(m map[string]*failCount, now time.Time) {
	for k, f := range m {
		if now.Sub(f.first) >= throttleWindow {
			delete(m, k)
		}
	}
}

// trust records that user signed in from network net.
func (t *throttle) trust(user, net string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.known == nil {
		t.known = map[string]map[string]time.Time{}
	}
	nets := t.known[user]
	if nets == nil {
		nets = map[string]time.Time{}
		t.known[user] = nets
	}
	nets[net] = now
	for len(nets) > knownNetsPerUser {
		oldest := ""
		for n, at := range nets {
			if oldest == "" || at.Before(nets[oldest]) {
				oldest = n
			}
		}
		delete(nets, oldest)
	}
}

// trusted reports whether user signed in from net recently. Guesses from
// such a network still count, but the per-user total doesn't stop it: an
// attacker elsewhere can't lock a person out of a network they use.
func (t *throttle) trusted(user, net string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.known[user][net]
	return ok && now.Sub(at) < knownFor
}

// forget drops what is remembered about user (deleted, or password reset).
func (t *throttle) forget(user string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.known, user)
}

// clientNet is the unit the throttle counts per: the address for IPv4,
// its /64 for IPv6 (one subscriber usually gets a whole /64).
func clientNet(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}
