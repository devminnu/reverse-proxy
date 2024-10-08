package proxy

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

	"reverse-proxy/config"
)

func ProxyHandler(cfg *config.Config) http.Handler {
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
