FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
# Copy the whole module (matches images/mcp-discord/Dockerfile): the sparse
# cmd-only COPY broke the v1.0.10 release when main.go grew an import of
# pkg/logging — and mcp images don't build on PRs, so the breakage only
# surfaced at release time.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/kyber-mcp-telegram ./cmd/kyber-mcp-telegram

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/kyber-mcp-telegram /usr/local/bin/kyber-mcp-telegram
ENTRYPOINT ["/usr/local/bin/kyber-mcp-telegram"]
