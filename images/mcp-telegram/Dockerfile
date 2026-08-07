FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/kyber-mcp-telegram ./cmd/kyber-mcp-telegram
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/kyber-mcp-telegram ./cmd/kyber-mcp-telegram

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/kyber-mcp-telegram /usr/local/bin/kyber-mcp-telegram
ENTRYPOINT ["/usr/local/bin/kyber-mcp-telegram"]
