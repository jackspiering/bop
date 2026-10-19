# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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
# `docker exec <container> bop ...` works.
RUN mkdir -p /out/tmp/bop /out/usr/local/bin && ln -s /bop /out/usr/local/bin/bop

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="bop" \
      org.opencontainers.image.description="Deliberately failing test image. Not for production." \
      org.opencontainers.image.source="https://github.com/jackspiering/bop" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/bop /bop
COPY --from=build /out/usr/ /usr/
COPY --from=build --chown=65532:65532 /out/tmp/bop /tmp/bop
USER 65532:65532
EXPOSE 8080
# --start-interval needs Docker Engine 25.0 or later.
HEALTHCHECK --interval=5s --timeout=2s --start-period=10s --start-interval=1s --retries=3 CMD ["/bop","healthcheck"]
ENTRYPOINT ["/bop"]
