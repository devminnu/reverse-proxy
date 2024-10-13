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

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

func main() {
	cfg := LoadConfig()
	httpMux := http.NewServeMux()

	// Handle /bridge/tonkeeper proxying with SSE stream forwarding
	httpMux.Handle("/bridge/tonkeeper", ProxyHandlerWithTimeout(cfg))

	// Create HTTP/2 server
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: httpMux,
		TLSConfig: &tls.Config{
			NextProtos:         []string{"h2", "http/1.1"}, // Support HTTP/2 and HTTP/1.1
			InsecureSkipVerify: true,                       // Bypass TLS verification for local testing
		},
	}

	// Enable HTTP/2 on the server
	http2.ConfigureServer(http2Server, &http2.Server{})

	// Create HTTP/3 server
	http3Server := &http3.Server{
		TLSConfig: loadTLSConfig(cfg.CertFile, cfg.KeyFile),
		Handler:   httpMux,
	}

	// Start UDP listener for HTTP/3
	udpListener, err := net.ListenPacket("udp", ":443")
	if err != nil {
		log.Fatalf("Failed to start UDP listener for HTTP/3: %v", err)
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

	// Start HTTP/3 server in a separate goroutine
	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/3 server failed: %v", err)
		}
	}()

	// Block until shutdown signal
	<-stop

	// Shutdown process
	log.Println("Shutting down servers...")

	// Shutdown HTTP/2 server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		if err := http2Server.Shutdown(shutdownCtx); err != nil {
			log.Println("Error while shutting down HTTP/2 server:", err)
		}

		// Shutdown HTTP/3 server
		if err := udpListener.Close(); err != nil {
			log.Println("Error while shutting down UDP listener:", err)
		}

		if err := http3Server.Close(); err != nil {
			log.Println("Error while shutting down HTTP/3 server:", err)
		}

		done <- true
	}()

	<-done
	log.Println("Servers gracefully stopped")
}

// ProxyHandlerWithTimeout proxies the incoming request to the backend and stops after a timeout
func ProxyHandlerWithTimeout(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Println("Invalid/unsupported backend URL")
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Custom RoundTripper to manage SSE and timeouts
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

	// Error handling
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if err == context.DeadlineExceeded {
			log.Println("Context deadline exceeded, closing backend response")
			w.WriteHeader(http.StatusGatewayTimeout)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use a context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		// Handle flushing the data stream to the client
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		cw := &contextAwareResponseWriter{w: w, ctx: ctx, flusher: flusher}

		// Handle real-time proxying
		proxy.ServeHTTP(cw, r)

		// Block until context is done
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				log.Println("Timeout reached, closing backend and client connections")

				// Ensure final response is sent to the client
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("Connection closed after timeout\n"))
				flusher.Flush()

				// Ensure graceful client closure
				time.Sleep(100 * time.Millisecond)
			}
		}
	})
}

// Custom RoundTripper that handles backend connections
type customRoundTripper struct {
	rt http.RoundTripper
}

func (c *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// Monitor context and close connection if timeout occurs
	go func() {
		select {
		case <-ctx.Done():
			log.Println("Context canceled, closing backend connection")
			resp.Body.Close() // Close connection gracefully
		}
	}()

	resp.Body = &contextAwareReadCloser{ctx: ctx, ReadCloser: resp.Body}
	return resp, nil
}

// Custom context-aware ReadCloser
type contextAwareReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (c *contextAwareReadCloser) Read(p []byte) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err() // Stop reading when context is done
	default:
		return c.ReadCloser.Read(p)
	}
}

// Custom response writer with context awareness
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
		c.flusher.Flush() // Ensure data is sent to the client immediately
		return n, err
	}
}

func (c *contextAwareResponseWriter) WriteHeader(statusCode int) {
	select {
	case <-c.ctx.Done():
		// Skip if context has been canceled
	default:
		c.w.WriteHeader(statusCode)
		c.flusher.Flush() // Flush headers to the client immediately
	}
}

// Config struct for storing configuration variables
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

// Helper function to get environment variables
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

func loadTLSConfig(certFile, keyFile string) *tls.Config {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load TLS certificates: %v", err)
	}

	return &tls.Config{
		MinVersion:         tls.VersionTLS13, // HTTP/3 requires at least TLS 1.3
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"h3"}, // Support HTTP/3 only
		InsecureSkipVerify: true,           // Allow self-signed certs for local testing
	}
}
