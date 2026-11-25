# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# BlobFish

> Ugly fish. Pretty storage.

BlobFish is a minimal, single-node, open-source object store written in Go. It stores files (PDFs, images, any blob) in buckets and serves them over a simple HTTP + JSON API. It is built for small and low-end servers (1 vCPU / 512 MB VPS, Raspberry Pi) where MinIO, SeaweedFS or Ceph are too heavy.

- Working title was "tinys3". The name is now **BlobFish**, command `blobfish`.
- License: **Apache 2.0**. Fully open source: no paid edition, no closed features.
- Status: milestones 1–7 done (skeleton, storage engine, /v1 API + auth, web UI + hardening). Storage gate: crash tests and simulated power-cut test (LazyFS) pass; directory-fsync loss is untested (needs dm-log-writes). Next: dogfooding (milestones 8–9).

## Why it exists

MinIO's community edition went into maintenance mode in Dec 2025 and its repo was archived in Feb 2026. Current alternatives (RustFS, SeaweedFS, Garage, Ceph) are all S3-compatible and mostly built around clustering. BlobFish targets the gap: **one binary, one data folder, a curl-friendly API, and a small web UI, on one small server.**

## Decisions (do not revisit without asking)

1. **Own simple API, not S3-compatible in core.** JSON responses, Bearer-token auth.
2. **S3 compatibility = optional module in v2**, built as another frontend on the same `Store` interface. Owned by the maintainer, community may extend it. Never put S3/SigV4 code in core packages.
3. **Single node only.** No clustering, replication or erasure coding, ever in core.
4. **Data safety is the headline feature.** Atomic writes, checksums, crash tests from day one.
5. Language: Go. Metadata: bbolt. Objects: plain files on disk.

## Non-goals for v1

Clustering/replication, versioning, lifecycle rules, object lock/WORM, IAM policies, LDAP/OIDC, vector search, image processing, analytics, S3 API in core.

## Architecture

Request flow, top to bottom:

```
Clients (apps via curl/SDK, browser, S3 tools in v2)
  -> HTTP router + middleware   (request ID, size limits, logging, recovery)
  -> Frontends                  (native API /v1 | web UI (embedded) | S3 module v2)
  -> Authenticator              (API keys, presigned URL signatures)
  -> Store interface            (the ONLY door to data)
  -> Engine                     (bbolt metadata index | blob files | janitor)
  -> Local disk (data/ folder)
```

Frontends must only call `Store` and `Authenticator`. They never touch bbolt or files directly. A new frontend (S3) only translates requests into `Store` calls.

### Store interface

```go
type Store interface {
    Put(ctx context.Context, bucket, key string, r io.Reader, meta Meta) (ObjectInfo, error)
    Get(ctx context.Context, bucket, key string, rng *Range) (io.ReadCloser, ObjectInfo, error)
    Head(ctx context.Context, bucket, key string) (ObjectInfo, error)
    Delete(ctx context.Context, bucket, key string) error
    List(ctx context.Context, bucket string, opts ListOptions) (ListResult, error)
}
```

Bucket create/delete/list live on the same engine (separate small interface or methods). `Authenticator` is also an interface so SigV4 can be plugged in later.

### On-disk layout

```
data/
  meta.db            # bbolt: buckets, object metadata, API key hashes
  objects/ab/cd/...  # object bytes, named by random ID, sharded by first 4 hex chars
  tmp/               # uploads in progress
```

Objects are stored under random IDs, not their keys, so any key is valid and long/odd names never hit filesystem limits. Metadata per object: size, content-type, SHA-256 ETag, modified time, user metadata (`X-Meta-*`), blob ID.

### Write path (must be crash-safe)

1. Stream body into `tmp/` while hashing (SHA-256). Never buffer whole files in RAM.
2. `fsync` the temp file.
3. Rename into `objects/` (atomic). fsync the directory.
4. Commit metadata in one bbolt transaction. On overwrite, old blob is removed after commit.
5. A crash at any step leaves either the old object or the new one, never a partial file.

**Janitor**: runs at startup and on a timer; deletes orphans in `tmp/` and blob files with no metadata row.

## v1 API

All endpoints under `/v1`, `Authorization: Bearer <key>`, JSON for everything except object bodies.

| Method | Path | Does |
| --- | --- | --- |
| PUT | `/v1/buckets/{bucket}` | Create bucket (admin) |
| DELETE | `/v1/buckets/{bucket}` | Delete bucket (must be empty) |
| PATCH | `/v1/buckets/{bucket}` | `{"public": true\|false}` (bucket admin). Public: object GET/HEAD work without a key; listing/writes still need one |
| GET | `/v1/buckets` | List buckets |
| PUT | `/v1/b/{bucket}/{key...}` | Upload object (raw body; Content-Type stored; optional `X-Meta-*`) |
| GET | `/v1/b/{bucket}/{key...}` | Download (supports `Range`, `If-None-Match`) |
| HEAD | `/v1/b/{bucket}/{key...}` | Object info |
| DELETE | `/v1/b/{bucket}/{key...}` | Delete (idempotent) |
| GET | `/v1/b/{bucket}?prefix=&delimiter=/&cursor=&limit=` | List objects, paginated |
| POST | `/v1/presign` | Presigned URL (bucket, key, method GET/PUT, expiry seconds) |
| POST/GET/DELETE | `/v1/links`, `/v1/links/{id}` | Share links: create (read access; `expiresIn` 0 = never), list per object/bucket, revoke (creator or bucket writer) |
| GET | `/s/{token}[/{name}]` | Serve a share link, no key. 404 `NoSuchLink` if revoked/deleted, 410 `LinkExpired` |
| GET | `/v1/whoami` | Key id, name, scopes, max object size (drives the web UI) |
| GET | `/v1/backup` | Consistent tar snapshot of everything (`*:admin`), streamed while the server runs |
| POST/GET/DELETE | `/v1/keys` | Manage API keys (admin) |
| GET | `/healthz` | Health check, no auth |

- **API keys**: random 32-byte tokens, only SHA-256 hash stored. Scopes like `invoices:rw`, `*:admin`.
- **Share links** live in bbolt (`links`, `linkidx`), token stored as-is so the UI can re-show links. Deleting an object deletes its links in the same transaction (a re-upload never revives them); overwriting keeps them.
- **Caching**: object responses send `Cache-Control: private, no-cache` (`public, no-cache` for anonymous public reads) so revocation and public→private apply immediately; ETag makes revalidation a cheap 304.
- **Presigned URLs**: query params `exp` + `sig`, where `sig = hex(HMAC-SHA256(server_secret, method NUL bucket NUL key NUL exp NUL))`. NUL separators keep it unambiguous (keys may contain anything but NUL). HEAD may use a GET signature. Max expiry 7 days. Server checks signature, then expiry; no key needed. The secret is generated once and stored in `meta.db` `settings`.
- **Errors**: one shape everywhere, stable codes:
  ```json
  { "error": { "code": "NoSuchKey", "message": "object not found", "requestId": "01J..." } }
  ```
- **Large files**: single streamed PUT up to a configurable limit (default 5 GB). Resumable multipart = v1.1.

## Milestones

| Weeks | Phase | Scope |
| --- | --- | --- |
| 1 | Skeleton | Repo, config file, `/healthz`, logging, CI building one static binary |
| 2-3 | Storage engine | `Store` interface, atomic writes, bbolt index, SHA-256 ETags |
| gate | | Crash and power-cut tests pass, no lost or partial objects |
| 4-5 | API and auth | `/v1` endpoints, scoped API keys, presigned URLs, error format |
| 6-7 | Web UI and hardening | Browse/upload UI, Range requests, size limits, fuzz and load tests |
| 8-9 | Dogfood | Run BlobFish behind the maintainer's own apps, fix what real use finds |
| gate | | Two weeks of real use with no data issues |
| 10 | Release v1.0 | Docs, Docker image, Linux amd64/arm64 binaries |
| later | v1.1 / v2 | Multipart uploads / optional S3 module with SigV4 |

## Commands

Module `github.com/nitishkdas/blobfish`, Go 1.27 (installed locally at `~/.local/go/bin`).

```sh
CGO_ENABLED=0 go build -o blobfish ./cmd/blobfish   # static binary
go test -race -count=1 ./...                        # full suite; must pass
go test -race -run TestLoad/env ./internal/config   # single test / subtest
go vet ./... && gofmt -l .                          # CI fails on either
go test -short ./...                                # skip kill -9 and 256 MB streaming tests
go test -run '^$' -fuzz '^FuzzList$' -fuzztime 60s ./internal/engine   # one fuzz target at a time
BLOBFISH_LOADTEST=1 go test -run TestLoadSmallMachine -v ./internal/server   # 1-CPU load report
node --check internal/webui/static/app.js           # UI has no build step; this is its only lint
test/powercut/run.sh -rounds 30                     # simulated power cuts (Docker + LazyFS); exit 0 pass, 1 fail, 2 inconclusive
```

Power-cut test (`test/powercut/`, see its README): blobfish's data dir on LazyFS inside Docker; each round SIGKILLs blobfish, drops all unsynced data (`lazyfs::clear-cache`), remounts, restarts and checks every key against a ledger of acknowledged requests. It catches a missing blob fsync or bbolt `NoSync`; it cannot catch a missing *directory* fsync (LazyFS passes metadata ops straight through). LazyFS runs with `-s` because multi-threaded mode segfaults.

Fuzz targets: `FuzzRangeResolve`, `FuzzValidateKey`, `FuzzValidateBucketName` (store), `FuzzParseRange`, `FuzzETagMatches` (server), `FuzzList` (engine, checks pagination against an in-memory model).

CI (`.github/workflows/ci.yml`) runs gofmt, vet, race tests, then builds linux amd64/arm64 static binaries.

## Code layout

- `cmd/blobfish` – thin `main`; `run()` holds the logic so it is testable (flags, config, listener, graceful shutdown on SIGTERM).
- `internal/config` – defaults → JSON file (`-config` / `BLOBFISH_CONFIG`, unknown fields rejected) → `BLOBFISH_*` env. `Load` takes `getenv` explicitly.
- `internal/server` – router, middleware chain (request ID → access log → recover → body limit), and the `/v1` handlers. `errors.go` holds the stable error codes and the `errorMapping` table: the one place internal errors become API codes. Add new sentinel errors there.
  - `/v1/b/...` object routes deliberately bypass `ServeMux` (it would clean/redirect keys like `a//b`); `api.objects` splits bucket/key from the raw decoded path.
  - Every handler calls `a.authorize(...)` before touching the store, so a 403 never reveals whether a bucket/key exists.
  - A checksum failure discovered mid-download panics with `http.ErrAbortHandler` to cut the connection (headers are already sent).
  - Object responses with script-capable types (HTML, SVG, XML, `+xml`, unparseable) get `Content-Security-Policy: sandbox` so an uploaded file can't run script on the UI's origin and read its stored key. Not applied to PDFs/images: Chrome won't render sandboxed PDFs.
  - `TestStreamingMemory` moves 256 MB through HTTP and fails if heap grows > 48 MB; it catches any `io.ReadAll` on bodies.
- `internal/webui` – embedded static UI at `/ui/` (`/` redirects there). Plain HTML/CSS + vanilla ES modules in `static/js/`, no build step, no framework (decided: revisit Preact+htm only if the UI roughly doubles). Modules: `dom.js` (`h()`, icons, toasts, `ask()`/`confirmDialog()` styled dialogs), `api.js` (fetch, `ApiError`, key storage), `state.js` (shared state, `can(perm, bucket)` mirroring `auth.Principal.Can`, `ui.*` re-render hooks so modules don't import each other), `router.js` (hash routes `#/b/<bucket>/<folder>/?page=&size=`, `#/keys`), `files.js`, `uploads.js`, `keys.js`, `app.js` (entry).
  - Strict CSP (no inline script/style; CSSOM writes like `el.style.x` are fine). Build DOM with `h()`/`textContent`; `TestNoUnsafeDOM` rejects `innerHTML`, `eval`, inline handlers and native `alert/confirm/prompt`.
  - Pagination is cursor-based: `state.listing.cursors[i]` fetches page i+1; deep links walk earlier pages to learn cursors. No total count exists.
  - `GET /v1/whoami` (name, scopes, maxObjectSize) drives which buttons show; the server still enforces everything.
  - Uploads: one at a time, overwrite prompt (Replace/Skip/Keep both via HEAD checks; skipped for write-only keys), folder upload keeps relative paths, client-side size-limit check.
  - Browsers normalize `.`/`..` path segments, so such keys are API-only.
- `internal/auth` – `Authenticator` interface (SigV4 can plug in later) and `Service`: bearer tokens (`bf_` + 32 random bytes, only SHA-256 stored via `store.Keys`), scopes `bucket:r|w|rw|admin` / `*:...`, presign/verify. `Principal.Can(perm, bucket, key)`; server-wide ops use bucket `"*"`, which only `*:` scopes match. Refuses deleting the last `*:admin` key. First start with no keys writes a bootstrap admin key to `<data_dir>/initial-admin-key` (0600; `cmd/blobfish/bootstrap.go`), never to the log (stderr lands in journald); it's also printed only when stderr is a TTY, and later starts warn while the file exists. `blobfish -new-admin-key` mints another offline.
- `internal/store` – `Store`/`Buckets` interfaces, value types, sentinel errors (`ErrNoSuchKey`, …), name/key/range validation. Frontends import only this.
- `internal/engine` – the implementation. bbolt buckets: `buckets` (name→record), `objects/<bucket>` (nested; key→JSON record with blob ID), `blobs` (index of committed blob IDs). A blob file not in `blobs` is an orphan; the janitor deletes it (at `Open` and hourly). Running Puts register tmp names/blob IDs in `inflight` so the janitor skips them; keep "track before rename, untrack after commit".
- Free-space reserve (`internal/engine/space.go`): `Put` refuses with `ErrInsufficientStorage` (HTTP 507) if `Meta.SizeHint` (from Content-Length) plus `min_free_space` won't fit, and `spaceGuard` re-checks every 64 MiB while streaming. Deletes are never blocked. `diskFree` uses `statfs` (unix); a file system reporting zero total blocks (LazyFS, some FUSE/network fs) means "unknown", not full, and the check is skipped, as on non-unix. Raw `ENOSPC` also maps to 507. Verified on a real 64 MB tmpfs filled to 0 bytes: deletes, restart and data all fine.
- Backups (`internal/engine/backup.go`): tar of `manifest.json`, `meta.db` (one bbolt read tx via `tx.WriteTo`) and every blob that snapshot references. While any backup runs, `removeBlob` queues deletions in `deferredRemovals` and the janitor deletes nothing, so no snapshot blob can vanish mid-copy (`TestBackupDuringWrites`; removing the pause makes it fail). `Restore` accepts only known entry names (no path traversal, regular files only), refuses non-empty targets, cleans up after itself on failure, then reads every object back through the checksum-verifying reader. CLI: `blobfish backup -o FILE` (server stopped; writes `.partial` then renames) and `blobfish restore -from FILE -data-dir DIR`. Backups contain key hashes, link tokens and the presign secret.
- Data format versioning (`internal/engine/format.go`): `settings/format_version` holds `FormatVersion` (currently 1). `Open` checks it read-only *before writing anything* and refuses newer data (`ErrFormatTooNew`, protects downgrades); unstamped data is format 1. Any change to meta.db or the objects/ layout that older binaries would misread must bump `FormatVersion` and add a step in `migrationSteps()`; all steps run in one bbolt transaction (all or nothing).
- Crash tests: `e.crash` hook stops a write dead at named points (`put:written|synced|renamed|committed`, `delete:committed`); `TestCrashPoints` reopens and checks old-or-new + no orphans. `TestKill9DuringWrites` SIGKILLs a child process (`TestCrashChild`) mid-write; skipped with `-short`. New write-path steps need a new crash point and table row.
- Reverse proxies (`internal/server/proxy.go`): `X-Forwarded-For`/`X-Forwarded-Proto` are believed only when the direct peer is in `trusted_proxies`; the client is the right-most untrusted XFF entry (left entries are client-forgeable). The access log's `remote` is that client, `proxy` the peer. `baseURL` (presign/share links) uses https only via TLS or a trusted proxy. HTTPS itself is Caddy's job: `docs/deploy.md` (Caddyfile, hardened systemd unit scoring 1.1 in `systemd-analyze security`, backups, upgrades), verified end to end with Caddy in Docker.
- Request IDs are ULIDs generated server-side (client IDs ignored), returned in `X-Request-Id` and in error bodies.
- `http.Server` has only `ReadHeaderTimeout`/`IdleTimeout`; no Read/Write timeouts because bodies are multi-GB streams. Instead `idleTimeoutMiddleware` (`internal/server/idle.go`) pushes the conn's read/write deadline forward on every body read and response write (`transfer_idle_timeout`, default 60s), so only transfers that stop making progress are dropped; a broken upload sets `Connection: close` (else net/http would wait forever discarding the unread body). A stalled upload is 408 `RequestTimeout`, not a logged 500. `LimitListener` caps open connections (`max_connections`, default 1000). `idle_test.go` covers this over real TCP.

## Engineering rules

- Keep it small. Prefer the standard library (`net/http`, `log/slog`, `crypto/*`, `embed`). Add a dependency only with a clear reason; bbolt is the expected core dependency.
- Stream everything. No `io.ReadAll` on object bodies.
- Every write path change needs a test that simulates a crash mid-write.
- Table-driven tests; `go test -race ./...` must pass. Use `t.TempDir()` for data dirs.
- Errors wrap with `%w`; map internal errors to API error codes in one place.
- No global state; pass config and dependencies explicitly.
- Static binary: `CGO_ENABLED=0`.
- Web UI assets embedded with `embed`, no Node build step required to run the server.

## Open items

- [ ] Claim GitHub repo/org and a domain (e.g. blobfish.dev / blobfish.io).
- [ ] Quick trademark search for "blobfish" (software classes 9 and 42).
- [ ] Mascot: original drawing of a sad pink blobfish holding a bucket (no copied art).
