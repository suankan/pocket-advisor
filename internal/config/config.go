// Package config resolves service configuration from the environment. Every
// binary reads the same variables so a single Helm values block configures the
// whole system.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type MinIO struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

type Postgres struct {
	DSN string
}

type NATS struct {
	URL string
}

type Embedding struct {
	Endpoint string
	APIKey   string
	Model    string
	Timeout  time.Duration
}

type Config struct {
	MinIO     MinIO
	Postgres  Postgres
	NATS      NATS
	Embedding Embedding

	MetricsPort int
	LogLevel    string
}

// Load reads configuration from the environment, applying the same defaults
// the Helm chart sets explicitly. Missing required values are reported all at
// once rather than one restart at a time.
func Load() (*Config, error) {
	c := &Config{
		MinIO: MinIO{
			Endpoint:  env("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey: env("MINIO_ACCESS_KEY", ""),
			SecretKey: env("MINIO_SECRET_KEY", ""),
			Bucket:    env("MINIO_BUCKET", "pocket-advisor"),
			UseSSL:    envBool("MINIO_USE_SSL", false),
		},
		Postgres: Postgres{
			DSN: env("POSTGRES_DSN", ""),
		},
		NATS: NATS{
			URL: env("NATS_URL", "nats://localhost:4222"),
		},
		Embedding: Embedding{
			Endpoint: env("EMBEDDING_ENDPOINT", ""),
			APIKey:   env("EMBEDDING_API_KEY", ""),
			Model:    env("EMBEDDING_MODEL", "jina-embeddings-v5-text-small"),
			Timeout:  envDuration("EMBEDDING_TIMEOUT", 60*time.Second),
		},
		MetricsPort: envInt("METRICS_PORT", 9090),
		LogLevel:    env("LOG_LEVEL", "info"),
	}
	return c, nil
}

// RequireMinIO validates the subset the uploader and discovery need.
func (c *Config) RequireMinIO() error {
	var missing []string
	if c.MinIO.AccessKey == "" {
		missing = append(missing, "MINIO_ACCESS_KEY")
	}
	if c.MinIO.SecretKey == "" {
		missing = append(missing, "MINIO_SECRET_KEY")
	}
	return report(missing)
}

// RequirePostgres validates the subset every stateful component needs.
func (c *Config) RequirePostgres() error {
	if c.Postgres.DSN == "" {
		return report([]string{"POSTGRES_DSN"})
	}
	return nil
}

func report(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required environment: %v", missing)
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
