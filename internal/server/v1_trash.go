package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Trash endpoints. Anyone who may delete objects in a bucket (write) may
// see its trash, restore from it and purge it: purging is no more than a
// delete with trash off would have done.

type trashItemJSON struct {
	ID      string     `json:"id"`
	Object  objectJSON `json:"object"`
	Deleted time.Time  `json:"deleted"`
	PurgeAt time.Time  `json:"purgeAt"`
}

type trashListJSON struct {
	Items      []trashItemJSON `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// trashDays is the configured retention in whole days, for clients.
func (a *api) trashDays() int {
	if a.trash == nil {
		return 0
	}
	return int(a.trash.TrashRetention() / (24 * time.Hour))
}

func (a *api) listTrash(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermWrite); !ok {
		return
	}
	q := r.URL.Query()
	opts := store.TrashListOptions{Cursor: q.Get("cursor")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			a.fail(w, r, badRequest("limit must be a positive integer"))
			return
		}
		opts.Limit = n
	}
	res, err := a.trash.ListTrash(r.Context(), bucket, opts)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := trashListJSON{Items: make([]trashItemJSON, 0, len(res.Items)), NextCursor: res.NextCursor}
	for _, it := range res.Items {
		out.Items = append(out.Items, trashItemJSON{
			ID: it.ID, Object: toObjectJSON(it.Object), Deleted: it.Deleted, PurgeAt: it.PurgeAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) restoreTrash(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermWrite); !ok {
		return
	}
	info, err := a.trash.RestoreTrash(r.Context(), bucket, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "object restored from trash",
		"request_id", RequestID(r.Context()), "bucket", bucket, "key", info.Key)
	writeJSON(w, http.StatusOK, toObjectJSON(info))
}

func (a *api) purgeTrashItem(w http.ResponseWriter, r *http.Request) {
	a.purge(w, r, r.PathValue("id"))
}

func (a *api) emptyTrash(w http.ResponseWriter, r *http.Request) {
	a.purge(w, r, "")
}

func (a *api) purge(w http.ResponseWriter, r *http.Request, id string) {
	bucket := r.PathValue("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermWrite); !ok {
		return
	}
	if err := a.trash.PurgeTrash(r.Context(), bucket, id); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "trash purged",
		"request_id", RequestID(r.Context()), "bucket", bucket, "id", id)
	w.WriteHeader(http.StatusNoContent)
}
