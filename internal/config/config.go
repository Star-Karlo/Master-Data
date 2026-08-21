// Package config loads the master data service configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Environment string
	LogLevel    string

	HTTPPort string
	GRPCPort string

	MongoURI      string
	MongoDatabase string
	MongoTimeout  time.Duration

	ServiceToken          string
	AcceptedServiceTokens []string

	CORSAllowedOrigins []string

	// CatalogCacheTTL controls how long global catalogue reads are cached in
	// process. Catalogues change rarely and are read on nearly every order
	// screen, so this removes most of the load without a separate cache tier.
	CatalogCacheTTL time.Duration
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	env := envOr("ENVIRONMENT", "development")

	cfg := &Config{
		Environment:        env,
		LogLevel:           envOr("LOG_LEVEL", "info"),
		HTTPPort:           envOr("HTTP_PORT", "5002"),
		GRPCPort:           envOr("GRPC_PORT", "6002"),
		MongoDatabase:      envOr("MONGO_DATABASE", "karlo_masterdata"),
		MongoTimeout:       durationOr("MONGO_TIMEOUT", 10*time.Second),
		CORSAllowedOrigins: splitOr("CORS_ALLOWED_ORIGINS", nil),
		CatalogCacheTTL:    durationOr("CATALOG_CACHE_TTL", 5*time.Minute),
	}

	var missing []string

	cfg.MongoURI = os.Getenv("MONGO_URI")
	if cfg.MongoURI == "" {
		if env == "development" {
			cfg.MongoURI = "mongodb://localhost:27017"
		} else {
			missing = append(missing, "MONGO_URI")
		}
	}

	cfg.ServiceToken = os.Getenv("SERVICE_TOKEN")
	if cfg.ServiceToken == "" {
		missing = append(missing, "SERVICE_TOKEN")
	}

	cfg.AcceptedServiceTokens = splitOr("ACCEPTED_SERVICE_TOKENS", nil)
	if len(cfg.AcceptedServiceTokens) == 0 {
		missing = append(missing, "ACCEPTED_SERVICE_TOKENS")
	}

	if len(cfg.CORSAllowedOrigins) == 0 {
		missing = append(missing, "CORS_ALLOWED_ORIGINS")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: required environment variables not set: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func (c *Config) IsProduction() bool { return c.Environment == "production" }

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func splitOr(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var _ = strconv.Atoi
