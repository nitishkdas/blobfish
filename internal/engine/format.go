package engine

import (
	"errors"
	"fmt"
	"strconv"

	"go.etcd.io/bbolt"
)

// FormatVersion is the on-disk format this binary reads and writes. Bump it
// whenever meta.db or the objects/ layout changes in a way older binaries
// would misread, and add the upgrade step to migrationSteps.
const FormatVersion = 6

// ErrFormatTooNew means the data dir was written by a newer BlobFish.
// Opening it with this binary could misread or damage it.
var ErrFormatTooNew = errors.New("data dir was written by a newer BlobFish")

var setFormat = []byte("format_version") // in bktSettings, decimal

// migration upgrades the data from format n to n+1 inside one transaction.
type migration func(tx *bbolt.Tx) error

// migrationSteps maps a format version to the step that upgrades it to
// the next one.
func migrationSteps() map[int]migration {
	return map[int]migration{
		// 2: trash. Adds the "trash" bucket and LinkRecord.Trashed. A
		// format-1 binary would serve links of trashed objects again
		// after a re-upload and never purge trash, so it must refuse.
		1: func(tx *bbolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(bktTrash)
			return err
		},
		// 3: per-bucket usage counters (for quotas). A format-2 binary
		// would write objects without updating them.
		2: recountUsage,
		// 4: upload links. A format-3 binary would keep a deleted bucket's
		// upload links, which a re-created bucket of that name would
		// then inherit.
		3: func(tx *bbolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(bktUplinks)
			return err
		},
		// 5: webhooks and their outbox. A format-4 binary would change
		// objects without queueing events, and keep a deleted bucket's
		// webhooks for a re-created bucket of that name.
		4: func(tx *bbolt.Tx) error {
			for _, b := range [][]byte{bktWebhooks, bktOutbox} {
				if _, err := tx.CreateBucketIfNotExists(b); err != nil {
					return err
				}
			}
			return nil
		},
		// 6: user emails and the email -> name index. A format-5 binary
		// would delete a user, or change their record, without updating
		// the index, leaving a stale entry that blocks the address and
		// signs in as whoever later takes the name.
		5: indexUserEmails,
	}
}

// readFormat returns the stamped format version, or 0 if there is none: a
// brand-new file, or one from before versioning (which is format 1).
func readFormat(tx *bbolt.Tx) (int, error) {
	settings := tx.Bucket(bktSettings)
	if settings == nil {
		return 0, nil
	}
	v := settings.Get(setFormat)
	if v == nil {
		return 0, nil
	}
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return 0, fmt.Errorf("corrupt format_version %q in meta.db", v)
	}
	return n, nil
}

// checkFormat refuses a data dir from a newer binary. It runs read-only,
// before Open writes anything, so a downgrade can never touch the data.
func checkFormat(db *bbolt.DB) error {
	return db.View(func(tx *bbolt.Tx) error {
		v, err := readFormat(tx)
		if err != nil {
			return err
		}
		if v > FormatVersion {
			return fmt.Errorf("%w: data format %d, this binary supports up to %d; install a newer blobfish",
				ErrFormatTooNew, v, FormatVersion)
		}
		return nil
	})
}

// upgradeFormat brings the data to target, running every step in order
// inside the caller's transaction: either all steps and the new stamp
// commit, or none do. It returns the version found before upgrading
// (0 for a new or pre-versioning data dir, treated as format 1).
func upgradeFormat(tx *bbolt.Tx, target int, steps map[int]migration) (int, error) {
	found, err := readFormat(tx)
	if err != nil {
		return 0, err
	}
	v := max(found, 1) // unstamped data predates versioning: it is format 1
	for ; v < target; v++ {
		step, ok := steps[v]
		if !ok {
			return found, fmt.Errorf("no upgrade step from data format %d to %d", v, v+1)
		}
		if err := step(tx); err != nil {
			return found, fmt.Errorf("upgrade data format %d to %d: %w", v, v+1, err)
		}
	}
	settings, err := tx.CreateBucketIfNotExists(bktSettings)
	if err != nil {
		return found, err
	}
	return found, settings.Put(setFormat, []byte(strconv.Itoa(target)))
}
