# LimitPlane — a single static binary in a scratch image.
#
# The Node deployment needed a runtime, a node_modules tree and the src/
# directory sitting next to the entrypoint. This needs one file. The dashboard
# HTML is compiled into the binary with go:embed, so there is nothing beside it
# that can go missing, and the final image has no shell, no package manager and
# no libc — which means it also has no CVEs from any of them.
#
# Build it for one architecture:
#     docker build -t limitplane:dev .
# Build it for both, the way CI publishes it:
#     docker buildx build --platform linux/amd64,linux/arm64 -t limitplane:v1 .

# ---- build ------------------------------------------------------------------
#
# --platform=$BUILDPLATFORM pins the builder to the NATIVE architecture and
# lets Go cross-compile to the target. The alternative — running an arm64
# builder under QEMU emulation on an amd64 host — works but takes minutes
# instead of seconds. Go's cross-compilation is free, so the emulator is pure
# waste. TARGETOS/TARGETARCH are supplied automatically by buildx.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none

WORKDIR /src

# No third-party dependencies, so there is no module download step to cache.
# Copying go.mod first still lets Docker skip re-resolving the toolchain.
COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/ ./web/

# CGO_ENABLED=0 is what makes the binary static and therefore runnable on
# scratch. -trimpath keeps build machine paths out of the artefact, and the
# ldflags strip the symbol table and DWARF info to cut the image size.
#
# The -X flags stamp the version into the binary, which then reports it as
# limitplane_build_info on /metrics. That matters more than it looks: the
# runtime image has no shell, so `kubectl exec` cannot tell you what is running
# in a pod. The metric is the only way to find out.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/limitplane ./cmd/gateway

# Verify the binary is genuinely static: a dynamically linked binary would
# fail at runtime on scratch with a confusing "no such file" error, and it is
# far cheaper to catch that here than in a crash loop. ldd can only inspect a
# NATIVE binary, so this is a no-op on a cross build — CGO_ENABLED=0 is the
# actual guarantee, and this is the belt to its braces.
RUN ! ldd /out/limitplane 2>/dev/null | grep -q "=>" || (echo "binary is not static" && exit 1)

# ---- runtime ----------------------------------------------------------------
FROM scratch

ARG VERSION=dev
ARG COMMIT=none

# OCI annotations. `docker inspect` and every registry UI read these, and the
# revision label is what ties a running image back to a git commit when
# something is on fire at 2am.
LABEL org.opencontainers.image.title="LimitPlane" \
      org.opencontainers.image.description="AI-aware multi-tenant rate limiting gateway and Kubernetes operator" \
      org.opencontainers.image.source="https://github.com/mightbeanshuu/LimitPlane" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

# The gateway makes outbound HTTPS calls (Groq, Stripe, the geo lookup), so it
# needs a CA bundle. scratch has none — this is the one thing you must copy.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/limitplane /limitplane

# Run unprivileged. There is no /etc/passwd in scratch, so the uid is numeric —
# 65534 is `nobody` on every mainstream base image. Declaring it here (rather
# than only in the Kubernetes securityContext) means `docker run` is safe too,
# and it lets the Deployment set runAsNonRoot: true, which the kubelet can only
# enforce when the image itself declares a numeric non-root user.
USER 65534:65534

ENV PORT=3000 DATA_DIR=/tmp
EXPOSE 3000

# The binary is its own health probe. A scratch image has no shell and no curl,
# so the usual `CMD curl -f ...` is impossible; `limitplane healthcheck` makes
# one local request and exits 0 or 1. Kubernetes does not use this — the
# kubelet makes the HTTP request itself via httpGet — but docker-compose and
# plain `docker run` do.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/limitplane", "healthcheck"]

ENTRYPOINT ["/limitplane"]
