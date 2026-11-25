#!/usr/bin/env bash
# Simulated power-cut test for BlobFish, using LazyFS inside Docker.
#
#   test/powercut/run.sh                      # 20 cuts, random seed
#   test/powercut/run.sh -rounds 50 -seed 42  # driver flags pass through
#   BLOBFISH_BIN=/path/to/other/blobfish test/powercut/run.sh
#                                             # test a different binary
#
# Needs Docker with access to /dev/fuse. The container gets SYS_ADMIN for
# its own FUSE mount; nothing on the host is mounted or changed.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../.." && pwd)"

mkdir -p "$here/build"
(cd "$repo" \
  && CGO_ENABLED=0 go build -o "$here/build/blobfish" ./cmd/blobfish \
  && CGO_ENABLED=0 go build -o "$here/build/driver" ./test/powercut/driver)

docker build -q -t blobfish-powercut "$here" >/dev/null

extra=()
if [[ -n "${BLOBFISH_BIN:-}" ]]; then
  extra=(-v "$(realpath "$BLOBFISH_BIN"):/opt/under-test/blobfish:ro")
  set -- -blobfish /opt/under-test/blobfish "$@"
fi

exec docker run --rm \
  --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  "${extra[@]}" blobfish-powercut "$@"
