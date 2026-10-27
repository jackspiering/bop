FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/bop ./cmd/bop
# distroless has no shell: stage the empty state dir and a PATH entry so that
# `docker exec <container> bop ...` works. The state dir is world-writable
# with the sticky bit, like /tmp, so sticky modes work under any UID.
RUN mkdir -p /out/tmp/bop /out/usr/local/bin && chmod 1777 /out/tmp/bop && ln -s /bop /out/usr/local/bin/bop

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
ARG VERSION=dev
# revision and created are set by publish.yml through metadata-action, which
# also overrides these labels; a local build has neither.
LABEL org.opencontainers.image.title="bop" \
      org.opencontainers.image.description="Deliberately failing test image. Not for production." \
      org.opencontainers.image.source="https://github.com/jackspiering/bop" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/bop /bop
COPY --from=build /out/usr/ /usr/
# Copy the parent: COPY of a directory copies its contents and creates the
# target 0755, which would lose the 1777 mode of /tmp/bop.
COPY --from=build --chown=65532:65532 /out/tmp/ /tmp/
USER 65532:65532
EXPOSE 8080
# --start-interval needs Dockerfile frontend 1.6+ (Engine 25.0+) to build; older engines ignore it at run time.
HEALTHCHECK --interval=5s --timeout=2s --start-period=10s --start-interval=1s --retries=3 CMD ["/bop","healthcheck"]
ENTRYPOINT ["/bop"]
