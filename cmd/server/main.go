package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reverse-proxy/config"
	"reverse-proxy/internal/app/handler"
	"syscall"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/http2"
)

func main() {
	cfg := config.GetConfig()
	log.Debug().Interface("config", cfg).Msg("app config")
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load TLS certificates")
	}
	httpMux := http.NewServeMux()
	// Handle /bridge/tonkeeper proxying with SSE stream forwarding
	httpMux.Handle("/bridge/tonkeeper", handler.ProxyHandlerWithTimeout(cfg))
	// Create HTTP/2 server
	http2Server := &http.Server{
		Addr:    fmt.Sprintf(":%v", cfg.Http2ServerPort),
		Handler: httpMux,
		TLSConfig: &tls.Config{
			Certificates:       []tls.Certificate{cert},
			NextProtos:         []string{"h2", "http/1.1"}, // Support HTTP/2 and HTTP/1.1
			InsecureSkipVerify: true,                       // Bypass TLS verification for local testing
		},
	}
	// Enable HTTP/2 on the server
	http2.ConfigureServer(http2Server, &http2.Server{})

	// Create HTTP/3 server
	http3Server := &http3.Server{
		TLSConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13, // HTTP/3 requires at least TLS 1.3
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
		log.Info().Msg("Starting HTTP/2 server on :9443")
		err := http2Server.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile)
		if err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("HTTP/2 server failed")
		}
	}()

	// Start HTTP/3 server in a separate goroutine
	go func() {
		log.Info().Msg("Starting HTTP/3 server on :443")
		err = http3Server.Serve(udpListener)
		if err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("HTTP/3 server failed")
		}
	}()

	// Block until shutdown signal
	<-stop

	// Shutdown process
	log.Info().Msg("Shutting down servers...")

	// Shutdown HTTP/2 server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
