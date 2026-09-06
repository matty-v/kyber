FROM golang:1.27-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/kyber-mcp-slack ./cmd/kyber-mcp-slack
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /build/kyber-mcp-slack /usr/local/bin/kyber-mcp-slack
ENTRYPOINT ["/usr/local/bin/kyber-mcp-slack"]
