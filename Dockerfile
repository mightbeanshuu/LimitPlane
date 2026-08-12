# LimitPlane — a single static binary in a scratch image.
#
# The Node deployment needed a runtime, a node_modules tree and the src/
# directory sitting next to the entrypoint. This needs one file. The dashboard
# HTML is compiled into the binary with go:embed, so there is nothing beside it
# that can go missing, and the final image has no shell, no package manager and
# no libc — which means it also has no CVEs from any of them.

# ---- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build

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
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/limitplane ./cmd/gateway

# Verify the binary is genuinely static: a dynamically linked binary would
# fail at runtime on scratch with a confusing "no such file" error, and it is
# far cheaper to catch that here than in a crash loop.
RUN ! ldd /out/limitplane 2>/dev/null | grep -q "=>" || (echo "binary is not static" && exit 1)

# ---- runtime ----------------------------------------------------------------
FROM scratch

# The gateway makes outbound HTTPS calls (Groq, Stripe, the geo lookup), so it
# needs a CA bundle. scratch has none — this is the one thing you must copy.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/limitplane /limitplane

# Run unprivileged. There is no /etc/passwd in scratch, so the uid is numeric.
USER 65534:65534

ENV PORT=3000 DATA_DIR=/tmp
EXPOSE 3000

ENTRYPOINT ["/limitplane"]
