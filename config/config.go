package config

import (
	"os"
	"strconv"
	"time"
)

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
