# Power-cut test

Checks that BlobFish never loses or corrupts an acknowledged write when the
machine loses power.

```sh
test/powercut/run.sh                      # 20 simulated power cuts
test/powercut/run.sh -rounds 50 -seed 42  # more cuts, reproducible
BLOBFISH_BIN=/path/to/blobfish test/powercut/run.sh   # test another build
```

Needs Docker and `/dev/fuse`. The container gets `SYS_ADMIN` for its own FUSE
mount and changes nothing on the host.

## How it works

BlobFish's data dir sits on [LazyFS](https://github.com/dsrhaslab/lazyfs), a
FUSE file system with its own page cache: written bytes reach the backing
disk only on `fsync`. Each round the driver (`driver/main.go`):

1. runs 8 clients that PUT (0–8 MB) and DELETE 40 keys, recording every
   response the server acknowledged;
2. at a random moment SIGKILLs blobfish, then sends `lazyfs::clear-cache`,
   which drops every unsynced byte, as a power cut drops the OS page cache;
3. remounts LazyFS (also clearing the kernel's FUSE cache) and restarts
   blobfish, whose janitor runs on startup;
4. checks every key: acknowledged writes read back byte for byte,
   acknowledged deletes stay deleted, requests in flight at the cut are
   either fully applied or not at all, no unknown keys, no orphan blob
   files, empty `tmp/`.

A plain `docker kill` would not be enough: containers share the host
kernel's page cache, so unsynced writes would survive the "crash".

## What it catches, and what it can't

Verified by running deliberately broken builds through it:

| Broken build | Result |
| --- | --- |
| Blob file not fsynced before commit | FAIL: acknowledged objects lose bytes; BlobFish's checksum check aborts the downloads |
| bbolt `NoSync` | FAIL: metadata rolls back past acknowledged writes; the janitor then deletes their blobs as orphans |

Limits:

- LazyFS passes metadata operations (create, rename, mkdir) straight to the
  backing disk, so a missing **directory** fsync is not detected. Testing
  that needs a block-level tool such as dm-log-writes.
- It models losing the OS page cache, not a drive that acknowledges flushes
  it never made. That is a hardware property (use drives with power-loss
  protection for important data).
- LazyFS runs single-threaded (`-s`): in multi-threaded mode 0.3.1
  segfaults inside its cache library (`libpcache.so`) under concurrent
  load. BlobFish and its clients stay concurrent; only the file system
  under it serializes operations.
- If LazyFS itself dies mid-run, the driver stops and exits with code 2,
  "INCONCLUSIVE". A crashing tool may have written anything to disk, so that
  run neither passes nor fails BlobFish. Exit 0 = pass, 1 = BlobFish failed.
- LazyFS 0.3.1 also aborts when it exits after unmount (an unjoined
  thread); the driver checks the unmount itself and ignores that status.
