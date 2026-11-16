FROM golang:1.27.4-bookworm AS build
ARG BUILD_REVISION=development
WORKDIR /src

COPY go.mod go.sum ./
# Optional public build-proxy CA; never added to the runtime trust store.
ARG BUILD_EXTRA_CA_CERT_BASE64
RUN set -eu; \
    if [ -n "${BUILD_EXTRA_CA_CERT_BASE64:-}" ]; then \
        ca_dir="$(mktemp -d)"; \
        trap 'rm -rf "$ca_dir"' EXIT HUP INT TERM; \
        printf '%s' "$BUILD_EXTRA_CA_CERT_BASE64" | base64 -d > "$ca_dir/extra.pem"; \
        cat /etc/ssl/certs/ca-certificates.crt "$ca_dir/extra.pem" > "$ca_dir/combined.pem"; \
        SSL_CERT_FILE="$ca_dir/combined.pem" go mod download; \
    else \
        go mod download; \
    fi
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X socialpublisher/internal/buildinfo.Revision=${BUILD_REVISION}" -o /out/publisher ./cmd/publisher

# cmd/publisher embeds the IANA timezone database through time/tzdata.
# Keep the HTTPS trust store while running the static binary without a shell.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/publisher /publisher
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/publisher"]
CMD ["serve"]
