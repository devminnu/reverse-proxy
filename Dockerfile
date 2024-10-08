# Use Golang for the build stage
FROM golang:1.19-alpine AS builder

WORKDIR /app

# Copy go.mod and go.sum and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source files and build the reverse proxy
COPY . .
RUN go build -o reverse-proxy

# Use a lightweight image for the runtime
FROM alpine:3.14

# Install required packages for HTTPS
RUN apk --no-cache add ca-certificates

# Copy the binary from the builder image
COPY --from=builder /app/reverse-proxy /usr/local/bin/reverse-proxy

# Expose the necessary ports (UDP 443 for HTTP/3)
EXPOSE 443/udp

# Entry point to run the reverse proxy
ENTRYPOINT ["/usr/local/bin/reverse-proxy"]
