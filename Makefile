
# Makefile for Reverse Proxy Server

# Environment variables
CERT_DIR=certs
CERT_FILE=$(CERT_DIR)/cert.pem
KEY_FILE=$(CERT_DIR)/key.pem
DOMAIN=localhost
BACKEND_URL=https://bridge.tonapi.io/bridge/events
REQUEST_TIMEOUT=60

# Go commands
GO=go
RUN=$(GO) run cmd/server/*.go

# Default target
all: run

# Run the reverse proxy
run: certificates
	@echo "Starting Reverse Proxy..."
	@BACKEND_URL=$(BACKEND_URL) REQUEST_TIMEOUT=$(REQUEST_TIMEOUT) TLS_CERT_FILE=$(CERT_FILE) TLS_KEY_FILE=$(KEY_FILE) $(RUN)

# Generate self-signed certificates
certificates: $(CERT_FILE) $(KEY_FILE)

$(CERT_FILE) $(KEY_FILE):
	@mkdir -p $(CERT_DIR)
	@echo "Generating self-signed TLS certificates..."
	openssl req -x509 -newkey rsa:4096 -sha256 -nodes -keyout $(KEY_FILE) -out $(CERT_FILE) -subj "/CN=$(DOMAIN)" -days 365
	@echo "Certificates generated at $(CERT_DIR)"

# Clean certificates
clean:
	@echo "Cleaning up certificates..."
	@rm -rf $(CERT_DIR)


# Help
help:
	@echo "Makefile commands:"
	@echo "  make run          - Run the reverse proxy server"
	@echo "  make certificates - Generate self-signed TLS certificates"
	@echo "  make clean        - Clean up generated certificates"

.PHONY: all run certificates clean help
