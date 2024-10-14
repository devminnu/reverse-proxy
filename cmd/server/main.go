package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/http2"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	log.Logger = log.Output(os.Stdout).With().Caller().Logger().With().Timestamp().Logger()

	cfg := LoadConfig()
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load TLS certificates")
	}

	// Create a ServeMux for handling requests
	httpMux := http.NewServeMux()
	httpMux.Handle("/bridge/tonkeeper", ProxyHandlerWithTimeout(cfg))

	// Create HTTP/2 server
	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: httpMux,
		TLSConfig: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: true, // For local testing with self-signed cert
		},
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}

	// Enable HTTP/2 on the server
	http2.ConfigureServer(http2Server, &http2.Server{})

	// Create HTTP/3 server
	http3Server := &http3.Server{
		TLSConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{"h3"}, // Support HTTP/3 only
			InsecureSkipVerify: true,           // Allow self-signed certs for local testing
		},
		Handler: httpMux,
	}

	// Start UDP listener for HTTP/3
	udpListener, err := net.ListenPacket("udp", ":443")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start UDP listener for HTTP/3")
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
			log.Fatal().Err(err).Msg("HTTP/2 server failed")
		}
	}()

	// Start HTTP/3 server in a separate goroutine
	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP/3 server failed")
		}
	}()

	// Block until shutdown signal
	<-stop

	// Graceful shutdown process
	log.Info().Msg("Shutting down servers...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := http2Server.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("Error while shutting down HTTP/2 server")
		}
		if err := udpListener.Close(); err != nil {
			log.Error().Err(err).Msg("Error while shutting down UDP listener")
		}
		if err := http3Server.Close(); err != nil {
			log.Error().Err(err).Msg("Error while shutting down HTTP/3 server")
		}
		done <- true
	}()

	<-done
	log.Info().Msg("Servers gracefully stopped")
}

// ProxyHandlerWithTimeout proxies the incoming request to the backend and stops after a timeout
func ProxyHandlerWithTimeout(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid backend URL")
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	// Custom RoundTripper to manage SSE and timeouts
	proxy.Transport = &customRoundTripper{
		rt: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   3 * time.Second,
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

	// Error handler to properly manage errors during proxying
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Error().Err(err).Msg("Error in proxy")
		if err == context.DeadlineExceeded {
			// http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
		} else {
			// http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // Disable buffering in proxies

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Create a context with timeout to cancel the backend request
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()

		// Custom response writer to handle the SSE streaming
		cw := &contextAwareResponseWriter{w: w, ctx: ctx, flusher: flusher}
		proxy.ServeHTTP(cw, r)

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				// Properly close the SSE connection on timeout
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("event: close\ndata: Connection closed after timeout\n\n"))
				flusher.Flush()
			}
		}
	})
}

// Custom RoundTripper for backend handling
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

func (c *contextAwareReadCloser) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
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
		if err != nil {
			return 0, err
		}
		c.flusher.Flush()
		return n, nil
	}
}

func (c *contextAwareResponseWriter) WriteHeader(statusCode int) {
	select {
	case <-c.ctx.Done():
		// Skip if context has been canceled
	default:
		c.w.WriteHeader(statusCode)
		c.flusher.Flush()
	}
}

// Config structure to hold configurations
type Config struct {
	BackendURL     string
	RequestTimeout time.Duration
	CertFile       string
	KeyFile        string
}

// LoadConfig loads configuration from environment variables
func LoadConfig() *Config {
	backendURL := getEnv("BACKEND_URL", "https://bridge.tonapi.io/bridge/events")
	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 10) // Default timeout of 10 seconds
	certFile := getEnv("TLS_CERT_FILE", "cert.pem")
	keyFile := getEnv("TLS_KEY_FILE", "key.pem")

	return &Config{
		BackendURL:     backendURL,
		RequestTimeout: time.Duration(requestTimeout) * time.Second,
		CertFile:       certFile,
		KeyFile:        keyFile,
	}
}

// Helper functions to load environment variables
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
