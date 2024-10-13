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
	httpMux.Handle("/bridge/tonkeeper", ProxyHandler(cfg))

	// Create HTTP/2 server
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: httpMux,
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

	// Load configuration
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("HTTP3 / request received")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello HTTP3\n"))
	})
	mux.Handle("/bridge/tonkeeper", ProxyHandler(cfg))

	// Create the HTTP/3 server
	http3Server := &http3.Server{
		TLSConfig: loadTLSConfig(cfg.CertFile, cfg.KeyFile),
		Handler:   mux,
	}

	// Channel to signal server shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	// Channel to ensure server is properly stopped
	done := make(chan bool, 1)

	// UDP listener for HTTP/3
	udpListener, err := net.ListenPacket("udp", ":443")
	if err != nil {
		log.Fatalf("Failed to start UDP listener for HTTP/3: %v", err)
	}

	// Start the HTTP/3 server in a separate goroutine
	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		log.Println("HTTP/3 server listening on UDP port 443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP/3 server failed: %v", err)
		}
	}()
	// Block until we receive the shutdown signal
	<-stop

	// Shutdown process begins
	log.Println("Shutting down server...")
	go func() {
		// Close the UDP listener to stop accepting new connections
		log.Println("closing the UDP listener...")
		if err := udpListener.Close(); err != nil {
			log.Println(err)
		}
		log.Println("closed udp listener")
		log.Println("shutting down server")
		if err := http3Server.Close(); err != nil {
			log.Println(err)
		}
		// Inform that the server is shutting down
		log.Println("server has been shut down")
		done <- true
	}()
	// Wait for the shutdown process to complete
	<-done
}

func loadTLSConfig(certFile, keyFile string) *tls.Config {
	// Load TLS certificates
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("failed to load TLS certificates: %v", err)

		return nil
	}
	// Create TLS configuration for HTTP/3
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13, // HTTP/3 requires at least TLS 1.3
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{"h3"}, // Support HTTP/3 only
		InsecureSkipVerify: false,
	}

	return tlsConfig
}

func ProxyHandler(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Println("invalid/unsupported backend url")
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
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

	proxy.ModifyResponse = func(res *http.Response) error {
		res.Header.Del("Origin")
		res.Header.Del("Referer")
		return nil
	}

	// Timeout wrapper
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println("request received")
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()

		// Use a context-aware request
		r = r.WithContext(ctx)

		// Start the proxy
		proxy.ServeHTTP(w, r)

		select {
		case <-ctx.Done():
			// If the timeout occurs, close the response
			log.Println("Request timed out")
			w.WriteHeader(http.StatusGatewayTimeout)
		default:
			// Continue serving the request until timeout happens
		}
	})
}

type Config struct {
	BackendURL     string
	RequestTimeout time.Duration
	CertFile       string
	KeyFile        string
}

func LoadConfig() *Config {
	backendURL := getEnv("BACKEND_URL", "https://bridge.tonapi.io/bridge/events")
	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 60) // Default timeout of 60 seconds
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
