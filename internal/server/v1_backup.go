package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
)

// backup streams a consistent tar snapshot of all data (server admins
// only). Uploads and downloads keep working while it runs. Restore it with
// `blobfish restore`, which also verifies every object's checksum.
func (a *api) backup(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	name := fmt.Sprintf("blobfish-backup-%s.tar", time.Now().UTC().Format("20060102-150405"))
	h := w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	h.Set("Cache-Control", "no-store") // contains key hashes and link tokens
	w.WriteHeader(http.StatusOK)
	if err := a.backuper.Backup(r.Context(), w); err != nil {
		// Headers are sent; cut the connection so the client sees a failed
		// download, not a tar that merely looks complete.
		a.log.ErrorContext(r.Context(), "backup failed", "request_id", RequestID(r.Context()), "err", err)
		panic(http.ErrAbortHandler)
	}
}
