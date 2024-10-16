package config

import (
	"encoding/json"
	"net/url"
	"time"

	env "github.com/Netflix/go-env"
	"github.com/rs/zerolog/log"
)

// URL is a custom type that wraps *url.URL to allow parsing via go-env
type URL struct {
	*url.URL
}

// UnmarshalText implements the text unmarshaling interface used by go-env
func (u *URL) UnmarshalEnvironmentValue(data string) error {
	parsedURL, err := url.Parse(data)
	if err != nil {
		return err
	}
	u.URL = parsedURL
	return nil
}

func (u *URL) MarshalEnvironmentValue() (string, error) {
	bytes, err := json.Marshal(u)
	if err != nil {
		return "", err
	}

	return string(bytes), nil
}

// Config struct for storing configuration variables
type Config struct {
	BackendURL      URL           `env:"BACKEND_URL,default=https://bridge.tonapi.io/bridge/events"`
	RequestTimeout  time.Duration `env:"REQUEST_TIMEOUT,default=60s"` // Now supports "60s", "1m", etc.
	CertFile        string        `env:"TLS_CERT_FILE,default=cert.pem"`
	KeyFile         string        `env:"TLS_KEY_FILE,default=key.pem"`
	Http2ServerPort string        `env:"HTTP2_SERVER_PORT,default=9443"`
	Http3ServerPort string        `env:"HTTP3_SERVER_PORT,default=443"`
}

var (
	cfg *Config
)

func init() {
	cfg = new(Config)
	// Load environment variables into the Config struct
	if _, err := env.UnmarshalFromEnviron(cfg); err != nil {
		log.Fatal().Err(err).Msg("Error loading environment variables")
	}
}

// LoadConfig loads configuration from environment variables
func GetConfig() *Config {
	return cfg
}
