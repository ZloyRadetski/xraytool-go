# Dockerfile
# Stage 1: Build the Go binary
FROM golang:alpine AS builder

# Set the working directory
WORKDIR /app

# Install git and other build dependencies
RUN apk add --no-cache git

# Copy go.mod and go.sum first to leverage Docker cache
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the binary with optimizations (-s -w strips debug info)
# CGO_ENABLED=0 ensures a static binary
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o xraytool main.go

# Stage 2: Create a minimal production image
FROM alpine:latest

# Add ca-certificates for HTTPS, util-linux for full nsenter support, and curl for downloads
RUN apk --no-cache add ca-certificates tzdata util-linux curl

WORKDIR /app

# Create necessary directories for volumes
RUN mkdir -p /etc/xraytool /var/lib/xraytool /usr/local/etc/xray

# Copy the static binary from the builder stage
COPY --from=builder /app/xraytool .

# Expose the default API port
EXPOSE 8080

# Create wrappers for systemctl and xray to allow the container to execute them on the host
RUN echo '#!/bin/sh' > /usr/local/bin/systemctl && \
    echo 'exec nsenter -t 1 -m -u -i -n -p systemctl "$@"' >> /usr/local/bin/systemctl && \
    chmod +x /usr/local/bin/systemctl && \
    echo '#!/bin/sh' > /usr/local/bin/xray && \
    echo 'exec nsenter -t 1 -m -u -i -n xray "$@"' >> /usr/local/bin/xray && \
    chmod +x /usr/local/bin/xray

# Run the binary
ENTRYPOINT ["./xraytool", "start-server"]
