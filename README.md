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
prints it to stderr **once**. Lost it? Stop the server and run
`./blobfish -new-admin-key` to mint another.

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

# Serve a whole bucket's files without a key (e.g. website assets):
curl -X PATCH -H "$H" -d '{"public":true}' localhost:8080/v1/buckets/site-assets
```

Scopes are `<bucket>:<r|w|rw|admin>`, with `*` for every bucket. Creating
buckets and managing keys needs `*:admin`. Errors always look like
`{"error":{"code":"NoSuchKey","message":"...","requestId":"..."}}`.

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

## License

Apache 2.0. See [LICENSE](LICENSE).
