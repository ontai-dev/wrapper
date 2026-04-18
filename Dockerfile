# Dockerfile — Wrapper operator (distroless).
#
# Wrapper is a long-running Deployment in seam-system. It manages ClusterPack
# compilation and delivery. Distroless: zero attack surface. INV-022.
# wrapper-schema.md §3.

FROM golang:1.25 AS builder
WORKDIR /build
COPY wrapper/ .
COPY conductor/ ../conductor/
COPY seam-core/ ../seam-core/
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /bin/wrapper \
    ./cmd/wrapper

FROM gcr.io/distroless/base:nonroot
COPY --from=builder /bin/wrapper /usr/local/bin/wrapper

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/wrapper"]
