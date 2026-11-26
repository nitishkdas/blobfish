# BlobFish

> Ugly fish. Pretty storage.

BlobFish is a small, single-node object store written in Go. It is one binary
and one data folder, with a curl-friendly HTTP + JSON API, built for small
servers (1 vCPU / 512 MB VPS, Raspberry Pi).

**Status:** early development. The storage engine, `/v1` API and web UI work,
but BlobFish has not been used with real data yet.

## Build and run

Requires Go 1.27+.

```sh
CGO_ENABLED=0 go build -o blobfish ./cmd/blobfish
./blobfish                      # listens on :8080, stores data in ./data
curl localhost:8080/healthz     # {"status":"ok"}
```

On first start, with no API keys yet, BlobFish creates an admin key and
saves it to `data/initial-admin-key`, a file only the BlobFish user can read
(it is also shown if you started BlobFish in a terminal). It is never written
to the log. Copy it somewhere safe and delete the file. Lost it? Stop the
server and run `./blobfish -new-admin-key` to mint another.

## Web UI

Open `http://localhost:8080/` and sign in with an API key. You can browse
buckets and folders page by page, open files in the browser, see details
(including the SHA-256 checksum), upload files or whole folders (by button
or drag and drop, with a prompt before overwriting), download, delete, and
make share links that expire (1 hour to 7 days). Admin keys also get an
API keys page to create and revoke keys scoped to buckets. Buttons a key
isn't allowed to use are hidden.

## Using the API

```sh
H="Authorization: Bearer bf_..."
curl -X PUT -H "$H" localhost:8080/v1/buckets/invoices
curl -X PUT -H "$H" -H 'Content-Type: application/pdf' -H 'X-Meta-Customer: acme' \
     --data-binary @inv.pdf localhost:8080/v1/b/invoices/2026/inv.pdf
curl -H "$H" localhost:8080/v1/b/invoices/2026/inv.pdf -o inv.pdf
curl -H "$H" 'localhost:8080/v1/b/invoices?prefix=2026/&delimiter=/'

# A key that can only read and write one bucket:
curl -X POST -H "$H" -d '{"name":"billing app","scopes":["invoices:rw"]}' localhost:8080/v1/keys

# A link anyone can use for one hour, no key needed:
curl -X POST -H "$H" -d '{"bucket":"invoices","key":"2026/inv.pdf","method":"GET","expiresIn":3600}' \
     localhost:8080/v1/presign

# Permanent link that anyone can open until you revoke it (expiresIn 0 = never):
curl -X POST -H "$H" -d '{"bucket":"invoices","key":"2026/inv.pdf","expiresIn":0}' localhost:8080/v1/links

# Same link, but the browser saves it as "Invoice 7.pdf" (add ?download to any
# object, presigned or share-link URL to force a download):
curl -X POST -H "$H" -d '{"bucket":"invoices","key":"2026/inv.pdf","filename":"Invoice 7.pdf"}' localhost:8080/v1/links

# Let someone without a key send you files (a web page at the returned url;
# they can only add files to that folder, never see or replace anything):
curl -X POST -H "$H" -d '{"bucket":"invoices","folder":"from-acme","label":"Invoices for ACME","maxFiles":20,"expiresIn":604800}' \
     localhost:8080/v1/uploadlinks

# Deleted by mistake? Deletes go to the bucket's trash for 7 days:
curl -H "$H" localhost:8080/v1/trash/invoices                      # list, newest first
curl -X POST -H "$H" localhost:8080/v1/trash/invoices/<id>/restore  # put it back

# Cap a bucket at 5 GiB (server admin key; 0 removes the cap). Uploads that
# don't fit get 507 QuotaExceeded; files in trash don't count:
curl -X PATCH -H "$H" -d '{"quotaBytes":5368709120}' localhost:8080/v1/buckets/invoices

# Serve a whole bucket's files without a key (e.g. website assets):
curl -X PATCH -H "$H" -d '{"public":true}' localhost:8080/v1/buckets/site-assets
```

Scopes are `<bucket>:<r|w|rw|admin>`, with `*` for every bucket. Creating
buckets and managing keys needs `*:admin`. Errors always look like
`{"error":{"code":"NoSuchKey","message":"...","requestId":"..."}}`.

## Uploading straight from your web pages

Your app's backend keeps the API key and only signs URLs; the user's browser
sends the file directly to BlobFish, so large uploads never pass through
your app. First allow your site on the bucket: in the web UI open the bucket,
click **Browser access** and add `https://app.example.com` (or send
`PATCH /v1/buckets/<bucket>` with `{"corsOrigins": ["https://app.example.com"]}`).
Each bucket has its own list, and changes apply immediately.

```js
// Backend (has the key): sign an upload URL for this user.
const r = await fetch("https://files.example.com/v1/presign", {
  method: "POST",
  headers: { Authorization: `Bearer ${process.env.BLOBFISH_KEY}`, "Content-Type": "application/json" },
  body: JSON.stringify({ bucket: "uploads", key: `users/${userId}/${crypto.randomUUID()}`, method: "PUT", expiresIn: 600 }),
});
const { url } = await r.json();   // send this URL to the browser

// Browser (no key): upload the picked file straight to BlobFish.
const res = await fetch(url, { method: "PUT", body: file, headers: { "Content-Type": file.type } });
const sha256 = res.headers.get("ETag");   // tell your backend it's done
```

Pages on other sites, or on your site but for other buckets, are refused by
the browser. Mobile apps, scripts and
servers aren't browsers, so they need no CORS setting at all.

## Production

BlobFish speaks plain HTTP: run it on `127.0.0.1` behind Caddy, which adds
HTTPS automatically. [docs/deploy.md](docs/deploy.md) has the complete setup
(config, a hardened systemd unit, Caddyfile, backups, upgrades), tested end
to end.

## Backups

BlobFish runs on one machine, so backups are your only second copy. A backup
is one `.tar` file: a consistent snapshot of the metadata and every file,
taken while the server keeps serving.

```sh
# Server running: download a backup with an admin key
curl -H "Authorization: Bearer bf_..." http://localhost:8080/v1/backup -o backup.tar

# Server stopped: back up the data dir directly
./blobfish backup -o backup.tar

# Restore into a new, empty folder; every file is checked against its
# SHA-256, so this is also how you verify a backup
./blobfish restore -from backup.tar -data-dir /srv/blobfish-restored
```

Backups contain API key hashes and share-link tokens (so keys and links keep
working after a restore): store them as carefully as the data itself.

## Checking for disk damage

Disks rot quietly. `blobfish verify` reads every file back from the disk and
compares it with the SHA-256 recorded when it was uploaded. It also checks
the metadata database. It changes nothing, and the server must be stopped.

```sh
sudo systemctl stop blobfish
./blobfish verify                 # or: -bucket invoices
sudo systemctl start blobfish
```

It prints one line per damaged file and exits 1 if it found any, so it can
run from cron. Restore damaged files from a backup or upload them again.

## Configuration

Settings come from built-in defaults, then an optional JSON file
(`-config path` or `BLOBFISH_CONFIG`), then environment variables.
See `blobfish.example.json`.

| Key | Env var | Default |
| --- | --- | --- |
| `listen` | `BLOBFISH_LISTEN` | `:8080` |
| `data_dir` | `BLOBFISH_DATA_DIR` | `data` |
| `max_object_size` (bytes) | `BLOBFISH_MAX_OBJECT_SIZE` | `5368709120` (5 GiB) |
| `log_level` | `BLOBFISH_LOG_LEVEL` | `info` |
| `log_format` (`text`/`json`) | `BLOBFISH_LOG_FORMAT` | `text` |
| `public_url` (base for presigned URLs) | `BLOBFISH_PUBLIC_URL` | request `Host` |
| `transfer_idle_timeout` (drop a transfer with no progress for this long) | `BLOBFISH_TRANSFER_IDLE_TIMEOUT` | `60s` |
| `max_connections` (extra clients wait in the accept queue) | `BLOBFISH_MAX_CONNECTIONS` | `1000` |
| `min_free_space` (bytes uploads never use, so deletes always fit; 0 = off) | `BLOBFISH_MIN_FREE_SPACE` | `536870912` (512 MiB) |
| `trash_days` (deleted files stay restorable this long; 0 = delete at once) | `BLOBFISH_TRASH_DAYS` | `7` |
| `trusted_proxies` (reverse proxies whose `X-Forwarded-For`/`-Proto` are believed) | `BLOBFISH_TRUSTED_PROXIES` (comma-separated) | none |

## License

Apache 2.0. See [LICENSE](LICENSE).
