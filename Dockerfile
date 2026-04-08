# Dockerfile — Wrapper operator (distroless).
#
# Wrapper is a long-running Deployment in seam-system. It manages ClusterPack
# compilation and delivery. Distroless: zero attack surface. INV-022.
# wrapper-schema.md §3.

FROM golang:1.25 AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /bin/ont-infra \
    ./cmd/ont-infra

FROM gcr.io/distroless/base:nonroot
COPY --from=builder /bin/ont-infra /usr/local/bin/ont-infra

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/ont-infra"]
