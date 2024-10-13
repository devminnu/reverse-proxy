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

	"github.com/gin-gonic/gin"
	"github.com/quic-go/quic-go/http3"
)

func main() {
	cfg := LoadConfig()

	// Initialize Gin router
	router := gin.Default()

	// Route to handle the /bridge/tonkeeper request with timeout handling
	router.Any("/bridge/tonkeeper", func(c *gin.Context) {
		ProxyHandlerWithTimeout(c, cfg)
	})

	// HTTP/2 server
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: router,
		TLSConfig: &tls.Config{
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: true,
		},
	}

	// HTTP/3 server
	http3Server := &http3.Server{
		TLSConfig: loadTLSConfig(cfg.CertFile, cfg.KeyFile),
		Handler:   router,
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
		if err := http2Server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/2 server failed: %v\n", err)
		}
	}()

	// Start HTTP/3 server
	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		if err := http3Server.Serve(udpListener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/3 server failed: %v", err)
		}
	}()

	// Block until shutdown signal
	<-stop

	// Shutdown both servers gracefully
	log.Println("Shutting down servers...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		// Shutdown HTTP/2
		if err := http2Server.Shutdown(shutdownCtx); err != nil {
			log.Println("Error while shutting down HTTP/2 server:", err)
		}

		// Shutdown HTTP/3
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

func ProxyHandlerWithTimeout(c *gin.Context, cfg *Config) {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid backend URL"})
		return
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

	// Custom error handler to suppress panic and handle timeout gracefully
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// Log the error but don't try to modify the response after headers are sent
		if err == context.DeadlineExceeded {
			log.Println("Context deadline exceeded, closing backend response")
			// Only send headers if they haven't been sent
			if !headersWereWritten(w) {
				http.Error(w, "Request timed out", http.StatusGatewayTimeout)
			}
		} else if err == http.ErrAbortHandler {
			// Handle abort panic gracefully, don't modify response
			log.Println("Request aborted")
		} else {
			log.Printf("Proxy error: %v", err)
			// Only send headers if they haven't been sent
			if !headersWereWritten(w) {
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}
	}

	// Create a context with a 20-second timeout
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	// Handle flushing the data stream to the client
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// Use a channel to monitor if headers were already written
	headersWritten := false

	// Proxy the request and manage timeout
	proxy.ServeHTTP(c.Writer, c.Request.WithContext(ctx))

	// Flush headers and check if they were written
	flusher.Flush()
	headersWritten = true

	// Handle context timeout or normal completion
	select {
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			log.Println("Timeout reached, closing backend and client connections")

			// Ensure no duplicate header write
			if !headersWritten {
				c.Writer.Header().Set("Content-Type", "text/event-stream")
				c.Writer.WriteHeader(http.StatusOK)
				_, _ = c.Writer.Write([]byte("Connection closed after timeout\n"))
				flusher.Flush()
			}

			// Graceful client closure
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// Helper function to check if headers were already written
func headersWereWritten(w http.ResponseWriter) bool {
	if rw, ok := w.(gin.ResponseWriter); ok {
		return rw.Written()
	}
	return false
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
