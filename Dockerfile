# Release image: the static binary on distroless (no shell, no package
# manager; it carries CA certificates for https webhooks, and its own
# Debian copyright files in /usr/share/doc). Built by the release workflow
# from binaries it has already built and tested, one per platform:
#
#   dist/linux-amd64/blobfish, dist/linux-arm64/blobfish
#
# To build one locally:
#   CGO_ENABLED=0 go build -trimpath -o dist/linux-amd64/blobfish ./cmd/blobfish
#   docker build --platform linux/amd64 -t blobfish .

# distroless has no shell, so the data folder is made here, owned by the
# image's nonroot user (65532): a fresh volume copies it, and BlobFish can
# write to /data without running as root.
FROM busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec AS datadir
RUN mkdir /data && chown 65532:65532 /data && chmod 0700 /data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETOS=linux
ARG TARGETARCH
COPY dist/${TARGETOS}-${TARGETARCH}/blobfish /usr/local/bin/blobfish
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/
COPY --from=datadir --chown=65532:65532 /data /data

# In a container, listen on every interface of the container's own
# network; publish the port only to your TLS proxy (see docs/deploy.md).
ENV BLOBFISH_LISTEN=:8080 \
    BLOBFISH_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/blobfish"]
