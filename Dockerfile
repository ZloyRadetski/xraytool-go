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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o xraytool main.go

# Stage 2: Create a minimal production image
FROM alpine:latest

# Add ca-certificates for HTTPS (required for Platega webhooks/API calls)
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Create necessary directories for volumes
RUN mkdir -p /etc/xraytool /var/lib/xraytool /usr/local/etc/xray

# Copy the static binary from the builder stage
COPY --from=builder /app/xraytool .

# Expose the default API port
EXPOSE 8080

# Create a wrapper for systemctl to allow the container to restart host services
RUN echo '#!/bin/sh' > /usr/local/bin/systemctl && \
    echo 'nsenter -t 1 -m -u -n -i systemctl "$@"' >> /usr/local/bin/systemctl && \
    chmod +x /usr/local/bin/systemctl

# Run the binary
ENTRYPOINT ["./xraytool"]
