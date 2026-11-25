# BlobFish

> Ugly fish. Pretty storage.

BlobFish is a small, single-node object store written in Go. It is one binary
and one data folder, with a curl-friendly HTTP + JSON API, built for small
servers (1 vCPU / 512 MB VPS, Raspberry Pi).

**Status:** early development. Only the server skeleton and `/healthz` exist so
far. Nothing here is ready for real data yet.

## Build and run

Requires Go 1.27+.

```sh
CGO_ENABLED=0 go build -o blobfish ./cmd/blobfish
./blobfish                      # listens on :8080, stores data in ./data
curl localhost:8080/healthz     # {"status":"ok"}
```

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

## License

Apache 2.0. See [LICENSE](LICENSE).
