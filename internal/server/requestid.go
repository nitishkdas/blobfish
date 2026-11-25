package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID returns the request ID stored in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// crockford is the Crockford base32 alphabet used by ULIDs.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newRequestID returns a 26-character ULID: 48 bits of Unix milliseconds
// followed by 80 random bits. IDs sort by time, which makes logs easy to
// scan, and need no dependency or shared state.
func newRequestID(now time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(now.UnixMilli())<<16)
	rand.Read(b[6:]) // crypto/rand.Read never returns an error since Go 1.24

	// Encode 128 bits as 26 base32 chars (the first char carries 3 bits).
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
