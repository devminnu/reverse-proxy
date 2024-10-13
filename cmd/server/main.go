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
	httpMux := http.NewServeMux()
	httpMux.Handle("/bridge/tonkeeper", ProxyHandlerWithTimeout(cfg))

	http2Server := &http.Server{
		Addr:    ":9443",
		Handler: httpMux,
		TLSConfig: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{"h2", "http/1.1"},
			InsecureSkipVerify: true,
		},
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	http2.ConfigureServer(http2Server, &http2.Server{})

	http3Server := &http3.Server{
		TLSConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{"h3"},
			InsecureSkipVerify: true,
		},
		Handler: httpMux,
	}

	udpListener, err := net.ListenPacket("udp", ":443")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start UDP listener for HTTP/3")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	done := make(chan bool, 1)

	go func() {
		fmt.Println("Starting HTTP/2 server on :9443")
		err := http2Server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP/2 server failed")
		}
	}()

	go func() {
		fmt.Println("Starting HTTP/3 server on :443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP/3 server failed")
		}
	}()

	<-stop

	log.Info().Msg("Shutting down servers...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

func ProxyHandlerWithTimeout(cfg *Config) http.Handler {
	targetURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Invalid backend URL")
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
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

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Error().Err(err).Msg("Error encountered by reverse proxy")
		if err == context.DeadlineExceeded {
			w.WriteHeader(http.StatusGatewayTimeout)
		} else {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()

		// Set proper SSE headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // Disable buffering in some proxies

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Use custom context-aware response writer to handle streaming and context cancellation
		cw := &contextAwareResponseWriter{w: w, ctx: ctx, flusher: flusher}
		proxy.ServeHTTP(cw, r)

		// Handle context timeout for closing connections gracefully
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				log.Info().Msg("Timeout reached, closing backend and client connections")

				// Send a final SSE event to notify the client of closure
				w.Write([]byte("event: close\ndata: Connection closed after timeout\n\n"))
				flusher.Flush()

				// Close the connection gracefully
				time.Sleep(100 * time.Millisecond) // Small delay to ensure client gets the final flush
			}
		}
	})

}

type customRoundTripper struct {
	rt http.RoundTripper
}

func (c *customRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	go func() {
		select {
		case <-ctx.Done():
			resp.Body.Close()
		}
	}()

	resp.Body = &contextAwareReadCloser{ctx: ctx, ReadCloser: resp.Body}

	return resp, nil
}

type contextAwareReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (c *contextAwareReadCloser) Read(p []byte) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		return c.ReadCloser.Read(p)
	}
}

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
	default:
		c.w.WriteHeader(statusCode)
		c.flusher.Flush()
	}
}

type Config struct {
	BackendURL     string
	RequestTimeout time.Duration
	CertFile       string
	KeyFile        string
}

func LoadConfig() *Config {
	backendURL := getEnv("BACKEND_URL", "https://bridge.tonapi.io/bridge/events")
	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 10)
	certFile := getEnv("TLS_CERT_FILE", "cert.pem")
	keyFile := getEnv("TLS_KEY_FILE", "key.pem")

	return &Config{
		BackendURL:     backendURL,
		RequestTimeout: time.Duration(requestTimeout) * time.Second,
		CertFile:       certFile,
		KeyFile:        keyFile,
	}
}

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
