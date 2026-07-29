// Package config resolves service configuration.
//
// Three layers, each overriding the one before: built-in defaults, the `infra`
// section of config.yaml, then the environment. Flags override all of it at the
// call site. Defaults alone are enough to run against a stock local cluster, so
// config.yaml exists to describe a different one rather than to repeat this one.
//
// The app runs on the host while its stores stay in the local OrbStack
// cluster, so the defaults are the cluster's own Service DNS names. OrbStack
// routes *.svc.cluster.local from macOS, but only through the system resolver
// — which Go consults only when cgo is enabled. mise.toml pins CGO_ENABLED=1
// for that as much as for Tesseract.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Cluster Service DNS defaults, matching a `pocket-advisor` release in the
// `pocket-advisor` namespace.
const (
	defaultRustFSEndpoint = "pocket-advisor-rustfs.pocket-advisor.svc.cluster.local:9000"
	defaultNATSURL        = "nats://pocket-advisor-nats.pocket-advisor.svc.cluster.local:4222"
	defaultPostgresDSN    = "postgres://postgres:postgrespassword@" +
		"pocket-advisor-postgres.pocket-advisor.svc.cluster.local:5432/" +
		"rag_ingestion?sslmode=disable"
	// The embedding endpoint is a plain localhost address now: the process that
	// calls it runs on the machine that serves it, so the host.docker.internal
	// indirection the in-cluster workers needed is gone.
	defaultEmbeddingEndpoint = "http://localhost:8000/v1/embeddings"

	// DefaultPath is where Load looks unless told otherwise.
	DefaultPath = "config.yaml"
)

// MinIO addresses RustFS, which implements the same S3 and admin wire
// protocols. The name is the client library's, not the server's.
type MinIO struct {
	Endpoint string
	Bucket   string
	UseSSL   bool

	// Two scoped identities, preserved even though one process now performs
	// both roles. The uploader is the only writer to raw/ and the only identity
	// permitted to delete; everything else reads anywhere but writes only under
	// extracted/. Collapsing them into one root credential would demote a
	// server-enforced policy back to a convention (§5.1).
	UploaderAccessKey string
	UploaderSecretKey string
	WorkerAccessKey   string
	WorkerSecretKey   string
}

type Postgres struct {
	DSN string
	// MaxConns must cover every lane that can hold a connection at once. The
	// pgxpool default is max(4, NumCPU), far below the lane count once all
	// roles share a process.
	MaxConns int32
}

type NATS struct {
	URL string
}

type Embedding struct {
	Endpoint    string
	APIKey      string
	Model       string
	Timeout     time.Duration
	Concurrency int
}

type Config struct {
	MinIO     MinIO
	Postgres  Postgres
	NATS      NATS
	Embedding Embedding

	MetricsPort int
	LogLevel    string
	// LogDir holds one file per role. The terminal belongs to the dashboard
	// while a run is live, so the files are the only full record.
	LogDir string
}

// file mirrors only the `infra` section of config.yaml. The rest of that file
// predates this system; unknown keys are ignored rather than rejected so the
// legacy sections stay harmless.
type file struct {
	Infra struct {
		RustFS struct {
			Endpoint          string `yaml:"endpoint"`
			Bucket            string `yaml:"bucket"`
			UseSSL            *bool  `yaml:"use_ssl"`
			UploaderAccessKey string `yaml:"uploader_access_key"`
			UploaderSecretKey string `yaml:"uploader_secret_key"`
			WorkerAccessKey   string `yaml:"worker_access_key"`
			WorkerSecretKey   string `yaml:"worker_secret_key"`
		} `yaml:"rustfs"`
		NATS struct {
			URL string `yaml:"url"`
		} `yaml:"nats"`
		Postgres struct {
			DSN      string `yaml:"dsn"`
			MaxConns int32  `yaml:"max_conns"`
		} `yaml:"postgres"`
		Embedding struct {
			Endpoint    string `yaml:"endpoint"`
			Model       string `yaml:"model"`
			Concurrency int    `yaml:"concurrency"`
			Timeout     string `yaml:"timeout"`
		} `yaml:"embedding"`
		Observability struct {
			MetricsPort int    `yaml:"metrics_port"`
			LogLevel    string `yaml:"log_level"`
			LogDir      string `yaml:"log_dir"`
		} `yaml:"observability"`
	} `yaml:"infra"`
}

// Load resolves configuration from defaults, then path, then the environment.
//
// A missing file is not an error — the defaults describe a stock local cluster.
// A malformed one is, because silently ingesting into the wrong store is worse
// than refusing to start.
func Load(path string) (*Config, error) {
	c := defaults()
	if err := applyFile(c, path); err != nil {
		return nil, err
	}
	applyEnv(c)
	return c, nil
}

func defaults() *Config {
	return &Config{
		MinIO: MinIO{
			Endpoint:          defaultRustFSEndpoint,
			Bucket:            "pocket-advisor",
			UseSSL:            false,
			UploaderAccessKey: "pa-uploader",
			UploaderSecretKey: "pa-uploader-secret",
			WorkerAccessKey:   "pa-worker",
			WorkerSecretKey:   "pa-worker-secret",
		},
		Postgres: Postgres{DSN: defaultPostgresDSN, MaxConns: 50},
		NATS:     NATS{URL: defaultNATSURL},
		Embedding: Embedding{
			Endpoint:    defaultEmbeddingEndpoint,
			Model:       "jina-embeddings-v5-text-small-mlx",
			Timeout:     60 * time.Second,
			Concurrency: 8,
		},
		MetricsPort: 9090,
		LogLevel:    "info",
		LogDir:      "logs",
	}
}

func applyFile(c *Config, path string) error {
	if path == "" {
		path = DefaultPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}

	var f file
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	in := f.Infra

	setStr(&c.MinIO.Endpoint, in.RustFS.Endpoint)
	setStr(&c.MinIO.Bucket, in.RustFS.Bucket)
	if in.RustFS.UseSSL != nil {
		c.MinIO.UseSSL = *in.RustFS.UseSSL
	}
	setStr(&c.MinIO.UploaderAccessKey, in.RustFS.UploaderAccessKey)
	setStr(&c.MinIO.UploaderSecretKey, in.RustFS.UploaderSecretKey)
	setStr(&c.MinIO.WorkerAccessKey, in.RustFS.WorkerAccessKey)
	setStr(&c.MinIO.WorkerSecretKey, in.RustFS.WorkerSecretKey)

	setStr(&c.NATS.URL, in.NATS.URL)

	setStr(&c.Postgres.DSN, in.Postgres.DSN)
	if in.Postgres.MaxConns > 0 {
		c.Postgres.MaxConns = in.Postgres.MaxConns
	}

	setStr(&c.Embedding.Endpoint, in.Embedding.Endpoint)
	setStr(&c.Embedding.Model, in.Embedding.Model)
	if in.Embedding.Concurrency > 0 {
		c.Embedding.Concurrency = in.Embedding.Concurrency
	}
	if in.Embedding.Timeout != "" {
		d, err := time.ParseDuration(in.Embedding.Timeout)
		if err != nil {
			return fmt.Errorf("%s: infra.embedding.timeout: %w", path, err)
		}
		c.Embedding.Timeout = d
	}

	if in.Observability.MetricsPort > 0 {
		c.MetricsPort = in.Observability.MetricsPort
	}
	setStr(&c.LogLevel, in.Observability.LogLevel)
	setStr(&c.LogDir, in.Observability.LogDir)
	return nil
}

// applyEnv lets the environment win over the file, so a one-off run can point
// somewhere else without editing committed configuration. Secrets in
// particular belong here rather than in a committed file.
func applyEnv(c *Config) {
	c.MinIO.Endpoint = env("RUSTFS_ENDPOINT", c.MinIO.Endpoint)
	c.MinIO.Bucket = env("RUSTFS_BUCKET", c.MinIO.Bucket)
	c.MinIO.UseSSL = envBool("RUSTFS_USE_SSL", c.MinIO.UseSSL)
	c.MinIO.UploaderAccessKey = env("RUSTFS_UPLOADER_ACCESS_KEY", c.MinIO.UploaderAccessKey)
	c.MinIO.UploaderSecretKey = env("RUSTFS_UPLOADER_SECRET_KEY", c.MinIO.UploaderSecretKey)
	c.MinIO.WorkerAccessKey = env("RUSTFS_WORKER_ACCESS_KEY", c.MinIO.WorkerAccessKey)
	c.MinIO.WorkerSecretKey = env("RUSTFS_WORKER_SECRET_KEY", c.MinIO.WorkerSecretKey)

	c.NATS.URL = env("NATS_URL", c.NATS.URL)

	c.Postgres.DSN = env("POSTGRES_DSN", c.Postgres.DSN)
	c.Postgres.MaxConns = int32(envInt("POSTGRES_MAX_CONNS", int(c.Postgres.MaxConns)))

	c.Embedding.Endpoint = env("EMBEDDING_ENDPOINT", c.Embedding.Endpoint)
	c.Embedding.APIKey = env("EMBEDDING_API_KEY", c.Embedding.APIKey)
	c.Embedding.Model = env("EMBEDDING_MODEL", c.Embedding.Model)
	c.Embedding.Timeout = envDuration("EMBEDDING_TIMEOUT", c.Embedding.Timeout)
	c.Embedding.Concurrency = envInt("EMBEDDING_CONCURRENCY", c.Embedding.Concurrency)

	c.MetricsPort = envInt("METRICS_PORT", c.MetricsPort)
	c.LogLevel = env("LOG_LEVEL", c.LogLevel)
	c.LogDir = env("LOG_DIR", c.LogDir)
}

// RequireMinIO validates the credentials for both scoped identities.
func (c *Config) RequireMinIO() error {
	var missing []string
	if c.MinIO.UploaderAccessKey == "" {
		missing = append(missing, "infra.rustfs.uploader_access_key")
	}
	if c.MinIO.UploaderSecretKey == "" {
		missing = append(missing, "infra.rustfs.uploader_secret_key")
	}
	if c.MinIO.WorkerAccessKey == "" {
		missing = append(missing, "infra.rustfs.worker_access_key")
	}
	if c.MinIO.WorkerSecretKey == "" {
		missing = append(missing, "infra.rustfs.worker_secret_key")
	}
	return report(missing)
}

// RequirePostgres validates the subset every stateful component needs.
func (c *Config) RequirePostgres() error {
	if c.Postgres.DSN == "" {
		return report([]string{"infra.postgres.dsn"})
	}
	return nil
}

// RequireEmbedding validates the subset the indexer and schema bootstrap need.
func (c *Config) RequireEmbedding() error {
	if c.Embedding.Endpoint == "" {
		return report([]string{"infra.embedding.endpoint"})
	}
	return nil
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func report(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required configuration: %v", missing)
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
