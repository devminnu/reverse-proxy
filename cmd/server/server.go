package main

// import (
// 	"os"
// 	"strconv"
// 	"time"
// )

// type Config struct {
// 	BackendURL     string
// 	RequestTimeout time.Duration
// 	CertFile       string
// 	KeyFile        string
// 	ReadTimeout    time.Duration
// 	WriteTimeout   time.Duration
// 	MaxIdleConns   uint32
// }

// func LoadConfig() *Config {
// 	backendURL := getEnv("BACKEND_URL", "https://bridge.tonapi.io/bridge/events")
// 	requestTimeout := getEnvAsInt("REQUEST_TIMEOUT", 15)
// 	certFile := getEnv("TLS_CERT_FILE", "cert.pem")
// 	keyFile := getEnv("TLS_KEY_FILE", "key.pem")

// 	readTimeout := getEnvAsInt("READ_TIMEOUT", 10)
// 	writeTimeout := getEnvAsInt("WRITE_TIMEOUT", 10)

// 	return &Config{
// 		BackendURL:     backendURL,
// 		RequestTimeout: time.Duration(requestTimeout) * time.Second,
// 		CertFile:       certFile,
// 		KeyFile:        keyFile,
// 		ReadTimeout:    time.Duration(readTimeout) * time.Second,
// 		WriteTimeout:   time.Duration(writeTimeout) * time.Second,
// 	}
// }

// func getEnv(key, defaultValue string) string {
// 	if value, exists := os.LookupEnv(key); exists {
// 		return value
// 	}
// 	return defaultValue
// }

// func getEnvAsInt(name string, defaultValue int) int {
// 	if valueStr, exists := os.LookupEnv(name); exists {
// 		if value, err := strconv.Atoi(valueStr); err == nil {
// 			return value
// 		}
// 	}
// 	return defaultValue
// }

// // func isClosedConnError(err error) bool {
// // 	if netErr, ok := err.(*net.OpError); ok && netErr.Err.Error() == "use of closed network connection" {
// // 		return true
// // 	}
// // 	return false
// // }
