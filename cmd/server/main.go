package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"reverse-proxy/config"
	"reverse-proxy/pkg/proxy"

	"github.com/quic-go/quic-go/http3"
)

func main() {
	// Load configuration
	cfg := config.LoadConfig()
	mux := http.NewServeMux()
	mux.Handle("/bridge/tonkeeper", proxy.ProxyHandler(cfg))

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
	// Create TLS configuration for HTTP/3
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13, // HTTP/3 requires at least TLS 1.3
		Certificates: make([]tls.Certificate, 1),
		NextProtos:   []string{"h3"}, // Support HTTP/3 only
	}
	// Load TLS certificates
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("failed to load TLS certificates: %v", err)
	}
	tlsConfig.Certificates[0] = cert

	return tlsConfig
}
