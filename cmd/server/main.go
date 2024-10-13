package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	cfg := LoadConfig()
	httpMux := http.NewServeMux()

	// Handle /bridge/tonkeeper proxying with SSE stream forwarding
	httpMux.Handle("/bridge/tonkeeper", ProxyHandlerWithTimeout(cfg))

	// HTTP/2 server with 20-second timeout
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: httpMux,
		TLSConfig: &tls.Config{
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: true, // Bypass TLS verification for local testing
		},
	}

	// Signal handling for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	done := make(chan bool, 1)

	// Start HTTP/2 server
	go func() {
		fmt.Println("Starting HTTP/2 server on :9443")
		err := http2Server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/2 server failed: %v\n", err)
		}
	}()

	// Block until shutdown signal
	<-stop

	// Shutdown process
	log.Println("Shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		// Gracefully shutdown the HTTP/2 server
		if err := http2Server.Shutdown(shutdownCtx); err != nil {
			log.Println("Error while shutting down HTTP/2 server:", err)
		}
		done <- true
	}()

	<-done
	log.Println("Server gracefully stopped")
}

// ProxyHandlerWithTimeout proxies the incoming request to the backend and stops after a timeout
func ProxyHandlerWithTimeout(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Println("Invalid/unsupported backend URL")
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Use a custom RoundTripper to handle timeouts and real-time streaming of SSE events
	proxy.Transport = &customRoundTripper{
		rt: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   20 * time.Second,
				KeepAlive: 20 * time.Second,
			}).DialContext,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	// Modify the request before forwarding to the backend
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = targetURL.Scheme
		req.URL.Host = targetURL.Host
		req.URL.Path = targetURL.Path

		query := req.URL.Query()
		if clientID := query.Get("pub"); clientID != "" {
			query.Set("client_id", clientID)
			query.Del("pub")
		}
		req.URL.RawQuery = query.Encode()

		req.Header.Del("Origin")
		req.Header.Del("Referer")
	}

	// Ensure real-time data forwarding from backend to client
	proxy.ModifyResponse = func(resp *http.Response) error {
		// Ensure that Transfer-Encoding is set to chunked to support real-time streaming
		resp.TransferEncoding = []string{"chunked"}
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set the context with a 20-second timeout
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		// Create a custom ResponseWriter to stream the data in real-time
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Wrap the response writer to handle context cancellation and streaming
		cw := &contextAwareResponseWriter{w: w, ctx: ctx, flusher: flusher}

		// Use context with timeout for the request
		r = r.WithContext(ctx)

		// Serve the proxy request, allowing the client to receive the SSE events in real-time
		proxy.ServeHTTP(cw, r)

		select {
		case <-ctx.Done():
			// When the context times out, ensure the connection is closed gracefully
			if ctx.Err() == context.DeadlineExceeded {
				log.Println("Timeout reached, closing connection with backend and sending 200 OK to client")
				// Send 200 OK and flush any remaining data
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("Connection closed successfully after 20 seconds\n"))
				flusher.Flush() // Ensure the client receives all data
				return
			}
		}
	})
}

// Custom RoundTripper that stops the backend stream after the timeout
type customRoundTripper struct {
	rt http.RoundTripper
}

func (c *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	// Make the actual backend request
	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Start a goroutine to monitor the context and close the connection when the context is canceled
	go func() {
		select {
		case <-ctx.Done():
			log.Println("Context canceled, closing backend response body")
			resp.Body.Close() // Gracefully close the response body when the context is canceled
		}
	}()

	// Wrap the response body with a reader that respects context cancellation
	resp.Body = &contextAwareReadCloser{ctx: ctx, ReadCloser: resp.Body}
	return resp, nil
}

// Custom context-aware ReadCloser to close the response body on context cancellation
type contextAwareReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (c *contextAwareReadCloser) Read(p []byte) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err() // Stop reading when the context is canceled
	default:
		return c.ReadCloser.Read(p)
	}
}

// Custom response writer that respects context cancellation and supports streaming
type contextAwareResponseWriter struct {
	w       http.ResponseWriter
	ctx     context.Context
	flusher http.Flusher
}

func (c *contextAwareResponseWriter) Header() http.Header {
	return c.w.Header()
}

func (c *contextAwareResponseWriter) Write(data []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		n, err := c.w.Write(data)
		c.flusher.Flush() // Ensure data is flushed to the client immediately
		return n, err
	}
}

func (c *contextAwareResponseWriter) WriteHeader(statusCode int) {
	select {
	case <-c.ctx.Done():
		// Don't write headers if the context is already canceled
	default:
		c.w.WriteHeader(statusCode)
		c.flusher.Flush() // Flush headers to the client immediately
	}
}

// Config struct to store configuration variables
type Config struct {
	BackendURL     string
	RequestTimeout time.Duration
	CertFile       string
	KeyFile        string
}

// LoadConfig from environment variables
func LoadConfig() *Config {
	backendURL := getEnv("BACKEND_URL", "https://bridge.tonapi.io/bridge/events")
	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 20) // Default timeout of 20 seconds
	certFile := getEnv("TLS_CERT_FILE", "cert.pem")
	keyFile := getEnv("TLS_KEY_FILE", "key.pem")

	return &Config{
		BackendURL:     backendURL,
		RequestTimeout: time.Duration(requestTimeout) * time.Second,
		CertFile:       certFile,
		KeyFile:        keyFile,
	}
}

// Helper functions to get environment variables
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvAsInt(name string, defaultValue int) int {
	if valueStr, exists := os.LookupEnv(name); exists {
		if value, err := strconv.Atoi(valueStr); err == nil {
			return value
		}
	}
	return defaultValue
}
