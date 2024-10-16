
# High Performance Reverse Proxy

This is a high-performance reverse proxy server implemented in Golang with support for both HTTP/2 and HTTP/3 protocols. This project handles long-lived Server-Sent Events (SSE) connections efficiently while modifying request headers and query parameters as per the proxy rules.

## Project Structure

```
reverse-proxy/
├── cmd/
│   └── server/
│       └── main.go              # Main entry point for the server
├── config/
│   └── config.go                # Configuration management
├── pkg/
│   └── proxy/
│       └── handler.go           # Reverse proxy handler
├── benchmarks/
│   └── benchmarks_test.go       # Benchmark tests for performance evaluation
├── go.mod                       # Go module file
└── go.sum                       # Go dependencies checksum
```

## Configuration

The reverse proxy uses environment variables for configuration. You can set the following variables:

- `BACKEND_URL`: The backend URL to which the requests should be proxied (default: `https://bridge.tonapi.io/bridge/events`)
- `REQUEST_TIMEOUT`: The request timeout in seconds (default: `60`)
- `TLS_CERT_FILE`: The path to the TLS certificate file (default: `cert.pem`)
- `TLS_KEY_FILE`: The path to the TLS key file (default: `key.pem`)

## Running the Reverse Proxy

1. **Set up environment variables**:
    ```bash
    export BACKEND_URL="https://your-backend.com/endpoint"
    export REQUEST_TIMEOUT=60s
    export TLS_CERT_FILE="path/to/cert.pem"
    export TLS_KEY_FILE="path/to/key.pem"
    ```

2. **Run the server**:

    You can run the server using the following command:

    ```bash
    go run cmd/server/main.go
    ```

    The server will start two instances:
    - **HTTP/2**: On port `8443`
    - **HTTP/3**: On port `443`

## HTTP/2 and HTTP/3 Setup

To fully support HTTP/3, make sure you have the following setup:

- **TLS Certificates**: You must use proper TLS certificates. For development purposes, self-signed certificates are okay, but for production, you should use trusted certificates like those from Let's Encrypt.

- **Port Configuration**: The server listens on:
  - `:8443` for HTTP/2 requests
  - `:443` for HTTP/3 requests

## Benchmarking

Benchmark tests are included to test the performance of the reverse proxy.

1. **Run Benchmarks**:

    You can run the benchmarks using the Go testing framework:

    ```bash
    go test -bench=. -benchmem ./benchmarks/
    ```

    This will output performance data such as the number of requests processed and memory allocations.

2. **Compare Benchmark Results**:

    You can compare benchmark results between different versions using the `benchstat` tool:

    - Install the tool:
      ```bash
      go install golang.org/x/perf/cmd/benchstat@latest
      ```
      
    - Run benchmarks:
      ```bash
      go test -bench=. -count=5 ./benchmarks/ > old.txt
      # Make your changes, then run benchmarks again:
      go test -bench=. -count=5 ./benchmarks/ > new.txt
      # Compare the results:
      benchstat old.txt new.txt
      ```

## Components Explanation

- **cmd/server/main.go**: This is the main entry point for the server. It sets up both HTTP/2 and HTTP/3 servers, loads the configuration, and handles graceful shutdowns.
  
- **config/config.go**: Handles loading configuration values from environment variables with default values if not provided.

- **pkg/proxy/handler.go**: This file contains the logic for the reverse proxy, including URL rewrites, header manipulation, and handling timeouts.

- **benchmarks/benchmarks_test.go**: Contains benchmark tests to evaluate the performance of the proxy server under load.

## Example of Usage

When a client requests `/bridge/tonkeeper?pub=test123`, the reverse proxy will:

- Forward the request to `https://bridge.tonapi.io/bridge/events`
- Change the `pub` query parameter to `client_id`
- Strip the `Origin` and `Referer` headers
- Terminate the connection after the configured `REQUEST_TIMEOUT` (default 60 seconds).

## HTTP/2 & HTTP/3 Testing

1. **HTTP/2 Testing**:
   - Use `curl` to test HTTP/2:
     ```bash
     curl -v --http2 https://localhost:8443/bridge/tonkeeper?pub=test123 --insecure
     ```

2. **HTTP/3 Testing**:
   - Use `curl` to test HTTP/3 (requires a recent version of `curl` with HTTP/3 support):
     ```bash
     curl -v --http3 https://localhost:443/bridge/tonkeeper?pub=test123 --insecure
     ```

Note: If using self-signed certificates, you may need to use the `--insecure` flag during testing. Replace `localhost` with your domain if you're using real certificates.

## Conclusion

This reverse proxy is designed for high performance in a production environment, supporting both HTTP/2 and HTTP/3. Use the benchmarking tools to evaluate the performance and scale the server as needed for your environment.

Feel free to customize the code to suit your use case.
