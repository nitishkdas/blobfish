# Power-cut test image: LazyFS (FUSE) + blobfish + the test driver.
# Build and run with ./run.sh, not directly.
FROM ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      g++ cmake make git ca-certificates pkg-config libfuse3-dev fuse3 \
 && apt-get clean

# LazyFS 0.3.1, pinned by commit so the test is reproducible.
ARG LAZYFS_COMMIT=045a0b3a1126725e693934e29d3ba15e08cc39ec
RUN git clone https://github.com/dsrhaslab/lazyfs.git /opt/lazyfs \
 && cd /opt/lazyfs && git checkout --quiet "$LAZYFS_COMMIT" \
 && cd libs/libpcache && ./build.sh \
 && cd ../../lazyfs && ./build.sh \
 && test -x /opt/lazyfs/lazyfs/build/lazyfs

RUN echo user_allow_other >> /etc/fuse.conf
COPY lazyfs.toml /etc/powercut/lazyfs.toml

# Static binaries built on the host by run.sh; copied last so code changes
# don't rebuild LazyFS.
COPY build/blobfish /usr/local/bin/blobfish
COPY build/driver /usr/local/bin/powercut-driver

ENTRYPOINT ["/usr/local/bin/powercut-driver"]
