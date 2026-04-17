# Buf stage (multi-arch official image)
FROM bufbuild/buf:1.47.2 AS buf

# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make curl

# Install buf from the official multi-arch image
COPY --from=buf /usr/local/bin/buf /usr/local/bin/buf

# Install protoc plugins (pinned for reproducibility)
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Generate proto files and build
RUN buf generate && \
    go build -ldflags="-w -s" -o /app/dist/forge ./cmd/forge

# Runtime stage
FROM alpine:3.21

# Install runtime dependencies
RUN apk add --no-cache ca-certificates git

# Create non-root user
RUN addgroup -g 1000 forge && \
    adduser -D -u 1000 -G forge forge

# Copy binary from builder
COPY --from=builder /app/dist/forge /usr/local/bin/forge

# Set ownership
RUN chown -R forge:forge /usr/local/bin/forge

USER forge
WORKDIR /workspace

ENTRYPOINT ["forge"]
CMD ["--help"]