package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func ProxyHandlerWithTimeout(backendURL string) http.Handler {
	targetURL, err := url.Parse(backendURL)
	if err != nil {
		panic(err)
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Modify the request before proxying
	proxy.Director = func(req *http.Request) {
		log.Debug().Msgf("Proxying request: %v %v", req.Method, req.URL.String())
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
		log.Debug().Msgf("Updated request URL: %v", req.URL.String())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set headers for SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no") // Disable buffering in some proxies

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		// Set up a context with a timeout of 10 seconds
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		// Channel to signal when the timeout occurs
		done := make(chan struct{})

		// Goroutine to handle timeout logic and send a final event before closing the stream
		go func() {
			<-ctx.Done() // Wait for the timeout
			if ctx.Err() == context.DeadlineExceeded {
				// Send a final event to the client signaling the end of the stream
				_, err := w.Write([]byte("event: stream-end\ndata: Stream complete with success\n\n"))
				if err != nil {
					log.Error().Err(err).Msg("Error sending success message")
				}
				flusher.Flush() // Ensure the final message is sent
				log.Debug().Msg("seinf finalllll")
				// Properly end the chunked transfer by sending the terminating chunk
				_, err = w.Write([]byte("0\r\n\r\n"))
				if err != nil {
					log.Error().Err(err).Msg("Error sending terminating chunk")
				}
				flusher.Flush() // Flush everything to avoid incomplete chunk encoding
			}
			close(done) // Signal the stream is closed
		}()

		// Proxy the request to the backend and stream the response to the client
		proxy.ServeHTTP(w, r)

		// Wait for the done signal before exiting to ensure no abrupt closure
		<-done
	})
}

func main() {
	backendURL := "https://bridge.tonapi.io/bridge/events"

	mux := http.NewServeMux()
	mux.Handle("/bridge/tonkeeper", ProxyHandlerWithTimeout(backendURL))

	server := &http.Server{
		Addr:         ":8080",
		Handler:      h2c.NewHandler(mux, &http2.Server{}), // Support HTTP/2 over cleartext (h2c)
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Graceful shutdown logic
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		fmt.Println("Starting server on :8080")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("ListenAndServe(): %v\n", err)
		}
	}()

	<-stop
	fmt.Println("Shutting down gracefully...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		fmt.Printf("Server Shutdown Failed:%+v", err)
	}

	fmt.Println("Server exited properly")
}
