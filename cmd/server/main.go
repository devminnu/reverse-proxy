package main

import (
	"context"
	"crypto/tls"
	"fmt"
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
)

func main() {
	cfg := LoadConfig()

	httpMux := http.NewServeMux()

	// HTTP2 and ping endpoints
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("HTTP2 / request received")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello HTTP2\n"))
	})
	httpMux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Ping request received")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong\n"))
	})

	// Handle /bridge/tonkeeper proxying
	httpMux.Handle("/bridge/tonkeeper", ProxyHandler(cfg))

	// HTTP/2 server with 30-second timeout
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: TimeoutHandler(httpMux, 30*time.Second),
		TLSConfig: &tls.Config{
			NextProtos:         []string{"h2", "http/1.1"}, // Negotiate both HTTP/1.1 and HTTP/2
			InsecureSkipVerify: false,
		},
	}

	// Start HTTP/2 server
	go func() {
		fmt.Println("Starting HTTP/2 server on :9443")
		err := http2Server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatalf("HTTP/2 server failed: %v\n", err)
		}
	}()

	// Load HTTP/3 configuration
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("HTTP3 / request received")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello HTTP3\n"))
	})
	mux.Handle("/bridge/tonkeeper", ProxyHandler(cfg))

	// Create HTTP/3 server with 30-second timeout
	http3Server := &http3.Server{
		TLSConfig: loadTLSConfig(cfg.CertFile, cfg.KeyFile),
		Handler:   TimeoutHandler(mux, 30*time.Second),
	}

	// Signal handling for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	done := make(chan bool, 1)

	// Start HTTP/3 listener
	udpListener, err := net.ListenPacket("udp", ":443")
	if err != nil {
		log.Fatalf("Failed to start UDP listener for HTTP/3: %v", err)
	}

	// Start HTTP/3 server in a separate goroutine
	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		log.Println("HTTP/3 server listening on UDP port 443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/3 server failed: %v", err)
		}
	}()

	// Block until shutdown signal
	<-stop

	// Shutdown process
	log.Println("Shutting down server...")
	go func() {
		if err := udpListener.Close(); err != nil {
			log.Println(err)
		}
		if err := http3Server.Close(); err != nil {
			log.Println(err)
		}
		done <- true
	}()
	<-done
}

func loadTLSConfig(certFile, keyFile string) *tls.Config {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("failed to load TLS certificates: %v", err)
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13, // HTTP/3 requires at least TLS 1.3
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"h3"}, // Support HTTP/3 only
		InsecureSkipVerify: false,
	}
}

// TimeoutHandler to enforce a 30-second timeout for connections
func TimeoutHandler(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		done := make(chan struct{})

		// Serve request in a separate goroutine
		go func() {
			next.ServeHTTP(w, r.WithContext(ctx))
			close(done)
		}()

		select {
		case <-ctx.Done():
			// Handle timeout
			if ctx.Err() == context.DeadlineExceeded {
				log.Println("Timeout reached, returning 200 OK and closing connection")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("Connection closed after 30 seconds\n"))
			}
		case <-done:
			// Request completed before the timeout
		}
	})
}

// ProxyHandler to handle forwarding requests to the backend and streaming SSE
func ProxyHandler(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Println("Invalid/unsupported backend URL")
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

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

	// Custom error handler for ReverseProxy to handle errors gracefully
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if err == context.DeadlineExceeded {
			log.Println("Proxy error: context deadline exceeded, returning 504 Gateway Timeout")
			http.Error(w, "Request timed out", http.StatusGatewayTimeout)
		} else {
			log.Printf("Proxy error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}

	// Custom transport that stops reading from the backend when context is canceled
	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// Handle backend SSE and ensure connection closure after 30 seconds
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		// Use context with timeout for the request
		r = r.WithContext(ctx)

		// Serve the proxy request
		proxy.ServeHTTP(w, r)

		// Handle context deadline exceeded or successful request completion
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				log.Println("Timeout: Closing both client and backend connections")
				return
			}
		}
	})
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
	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 30) // Default timeout of 30 seconds
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
