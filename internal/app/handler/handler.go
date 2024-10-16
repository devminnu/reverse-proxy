package handler

import (
	"context"
	"io"
	"net/http"
	"reverse-proxy/config"
	"time"

	"github.com/rs/zerolog/log"
)

func ProxyHandlerWithTimeout(cfg *config.Config) http.Handler {
	client := &http.Client{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, r.Method, cfg.BackendURL.URL.String(), r.Body)
		if err != nil {
			http.Error(w, "Failed to create request", http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		req.URL.RawQuery = r.URL.Query().Encode()

		query := req.URL.Query()
		if clientID := query.Get("pub"); clientID != "" {
			query.Set("client_id", clientID)
			query.Del("pub")
		}
		req.URL.RawQuery = query.Encode()

		req.Header.Del("Origin")
		req.Header.Del("Referer")
		log.Debug().Str("url", r.URL.String()).Msg("Target Backend URL")
		resp, err := client.Do(req)
		if err != nil {
			log.Error().Err(err).Msg("failed to connect to backend")
			http.Error(w, "Failed to connect to backend", http.StatusInternalServerError)

			return
		}
		defer resp.Body.Close()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		// w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					if _, writeErr := w.Write(buf[:n]); writeErr != nil {
						return
					}
					flusher.Flush()
				}
				if err != nil {
					if err == io.EOF {
						return
					}
					return
				}
			}
		}()
		select {
		case <-ctx.Done():
			log.Debug().Msg("Context timeout, stopping stream")
			w.Write([]byte("\n"))
			flusher.Flush()
		case <-done:
			log.Debug().Msg("Streaming completed")
		}
	})
}
