# BlobFish

> Ugly fish. Pretty storage.

BlobFish is a small, single-node object store written in Go. It is one binary
and one data folder, with a curl-friendly HTTP + JSON API, built for small
servers (1 vCPU / 512 MB VPS, Raspberry Pi).

**Status:** pre-1.0, being run against real apps (dogfooding) before the
v1.0 release. The storage engine, `/v1` API and web UI are complete. Writes
are atomic and checksummed; crash tests (a write killed at every step, and
`kill -9` mid-upload) and simulated power cuts pass. Losing a directory
entry in a power cut is not tested yet. Until v1.0, keep backups.

## Install

```sh
# Linux amd64/arm64: download a release, check its SHA-256, install the binary
curl -fsSL https://raw.githubusercontent.com/nitishkdas/blobfish/main/install.sh | sh
# ...or also set up a hardened systemd service on 127.0.0.1:8080:
curl -fsSL https://raw.githubusercontent.com/nitishkdas/blobfish/main/install.sh | sh -s -- --systemd

# Docker (data in a volume; put a TLS proxy in front, see docs/deploy.md)
docker run -d --name blobfish -p 127.0.0.1:8080:8080 -v blobfish:/data ghcr.io/nitishkdas/blobfish:latest

# From source (Go 1.27+)
CGO_ENABLED=0 go build -o blobfish ./cmd/blobfish
./blobfish                      # listens on 127.0.0.1:8080, stores data in ./data
curl localhost:8080/healthz     # {"status":"ok",...}
```

Release archives and the image carry `LICENSE`, `NOTICE` and
`THIRD_PARTY_NOTICES.md`.

On first start, with no API keys yet, BlobFish creates an admin key and
saves it to `data/initial-admin-key`, a file only the BlobFish user can read
(it is also shown if you started BlobFish in a terminal). It is never written
to the log. Copy it somewhere safe and delete the file. Lost it? Stop the
server and run `./blobfish -new-admin-key` to mint another. In Docker, read
it with `docker cp blobfish:/data/initial-admin-key -`.

## Web UI

Open `http://localhost:8080/` and sign in. You can browse buckets and
folders page by page, open files in the browser, see details (including
the SHA-256 checksum), upload files or whole folders (by button or drag and
drop, with a prompt before overwriting), download, delete, and make share
links. Buttons you aren't allowed to use are hidden. The UI is made for
desktop browsers (narrow windows scroll sideways); the upload-link page
works on phones too.

### Users and API keys

**People** sign in with a user name and password; **apps** use API keys.
Both get the same kind of access: per bucket read, write, read & write or
admin.

1. On first start BlobFish writes an admin API key to
   `<data_dir>/initial-admin-key`. Sign in with it ("Sign in with an API
   key instead"), open **Users** and add a user for yourself with access to
   *All buckets · Admin*. Then sign in with that user, store the key
   somewhere safe (or revoke it once you have another admin key) and delete
   the file.
2. Add a user for each person, with only the buckets they need. They can
   change their password from the lock button next to their name.

Sessions end after 12 hours without use and after 7 days at most. Changing
a password signs that user out everywhere else; admins can also sign a
user out everywhere. Wrong passwords are limited per 15 minutes: 10 per
user name from one network, 100 per user name in all, and 30 per network
(IPv6 counts per /64). Guesses are counted before they are checked, so
sending many at once doesn't get around the limits. Someone guessing your
name from one network can't lock you out; a guesser using many networks can
make new sign-ins wait up to 15 minutes, except from networks you signed in
from during the last 7 days (remembered until the server restarts). API
keys are never affected. Passwords are stored as PBKDF2-SHA256 hashes
(600,000 rounds).

Forgot the password and no admin can sign in? Stop the server and run
`blobfish -reset-password NAME`: it prints a new random password and ends
that user's sessions.

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
# they can only add files to that folder, never see or replace anything,
# and aren't told whether a file name was already taken):
curl -X POST -H "$H" -d '{"bucket":"invoices","folder":"from-acme","label":"Invoices for ACME","maxFiles":20,"expiresIn":604800}' \
     localhost:8080/v1/uploadlinks

# Deleted by mistake? Deletes go to the bucket's trash for 7 days:
curl -H "$H" localhost:8080/v1/trash/invoices                      # list, newest first
curl -X POST -H "$H" localhost:8080/v1/trash/invoices/<id>/restore  # put it back

# Users (server admin): add one, sign in as them, change your own password.
curl -X POST -H "$H" -d '{"name":"priya","password":"a long password","scopes":["invoices:rw"]}' localhost:8080/v1/users
curl -X POST -d '{"username":"priya","password":"a long password"}' localhost:8080/v1/login   # -> {"token":"bfs_..."}
curl -X POST -H "Authorization: Bearer bfs_..." -d '{"currentPassword":"...","newPassword":"..."}' localhost:8080/v1/password

# Cap a bucket at 5 GiB (server admin key; 0 removes the cap, the smallest
# cap is 10 MiB). Uploads that don't fit get 507 QuotaExceeded; files in
# trash don't count. The web UI takes the size in MB:
curl -X PATCH -H "$H" -d '{"quotaBytes":5368709120}' localhost:8080/v1/buckets/invoices

# Serve a whole bucket's files without a key (e.g. website assets):
curl -X PATCH -H "$H" -d '{"public":true}' localhost:8080/v1/buckets/site-assets
```

Scopes are `<bucket>:<r|w|rw|admin>`, with `*` for every bucket. Creating
buckets and managing keys needs `*:admin`. Errors always look like
`{"error":{"code":"NoSuchKey","message":"...","requestId":"..."}}`.

Share links and upload links act for whoever made them: they work only
while that key or user exists and still has access (read for share links,
write for upload links), and work again if access comes back.

Every endpoint, parameter and error code is in
[docs/api.md](docs/api.md).

## Webhooks

BlobFish can tell your app when files in a bucket are added, deleted or
restored. A bucket admin registers an address; the response carries a signing
secret, shown only once:

```sh
curl -X POST -H "$H" -d '{"bucket":"invoices","url":"https://app.example.com/hooks/blobfish"}' \
     localhost:8080/v1/webhooks
```

Each event is a JSON `POST` such as
`{"id":"…","type":"object.created","time":"…","bucket":"invoices","key":"2026/inv.pdf","size":48213,"etag":"…"}`
with `X-BlobFish-Event`, `X-BlobFish-Delivery` (the event id) and
`X-BlobFish-Signature: t=<unix time>,v1=<hex HMAC-SHA256 of "<t>.<body>">`.
Check it before trusting the body, for example in Python:

```python
t, v1 = (p.split("=", 1)[1] for p in request.headers["X-BlobFish-Signature"].split(","))
good = hmac.compare_digest(v1, hmac.new(secret, f"{t}.".encode() + body, "sha256").hexdigest())
fresh = abs(time.time() - int(t)) < 300   # reject replays
```

Events are saved together with the change they describe, so a crash or a
restart never loses one. If your app is down they wait (up to 10,000 per
webhook) and are retried with backoff for about a day, in order. An event can
arrive twice, so de-duplicate on its id. Webhooks created with a bucket admin
key may only call public addresses; a server admin key may also call an app on
the same machine (`http://127.0.0.1:…`).

## Uploading straight from your web pages

Your app's backend keeps the API key and only signs URLs; the user's browser
sends the file directly to BlobFish, so large uploads never pass through
your app. First allow your site on the bucket: in the web UI open the bucket's
**Settings** tab, click **Browser access** and add `https://app.example.com` (or send
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

BlobFish speaks plain HTTP: run it on `127.0.0.1` (the default) behind
Caddy, which adds HTTPS automatically. Behind a trusted proxy BlobFish sends
HSTS itself. [docs/deploy.md](docs/deploy.md) has the complete setup
(config, a hardened systemd unit, Caddyfile, Docker, rotating off-machine
backups, upgrades and rollback), tested end to end.
[docs/privacy.md](docs/privacy.md) lists what BlobFish stores and logs, for
operators who must answer privacy questions. Report security problems as
described in [SECURITY.md](SECURITY.md).

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
| `listen` (`:8080` = all interfaces; needed in containers) | `BLOBFISH_LISTEN` | `127.0.0.1:8080` |
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
| `operator_name` (shown on upload-link pages: who receives the files) | `BLOBFISH_OPERATOR_NAME` | none |
| `privacy_url` (your privacy notice, linked from upload-link pages) | `BLOBFISH_PRIVACY_URL` | none |

## License

Copyright 2026 The BlobFish Authors. Licensed under the Apache License 2.0;
see [LICENSE](LICENSE) and [NOTICE](NOTICE). Third-party licences are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Contributions: see
[CONTRIBUTING.md](CONTRIBUTING.md).
