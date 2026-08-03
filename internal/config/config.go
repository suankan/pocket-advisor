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
	"net/url"
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
	// The maintenance connection used only to CREATE/DROP per-workspace
	// databases and roles (workspace-isolation.md §3) — never used for
	// document data, which lives in a database per workspace.
	defaultPostgresAdminDSN = "postgres://postgres:postgrespassword@" +
		"pocket-advisor-postgres.pocket-advisor.svc.cluster.local:5432/" +
		"postgres?sslmode=disable"
	// The embedding endpoint is a plain localhost address now: the process that
	// calls it runs on the machine that serves it, so the host.docker.internal
	// indirection the in-cluster workers needed is gone.
	defaultEmbeddingEndpoint = "http://localhost:8000/v1/embeddings"
	defaultRerankEndpoint    = "http://localhost:8000/v1/rerank"
	defaultLLMEndpoint       = "http://localhost:8000/v1/chat/completions"

	// Matches the CLI's own --workspace-config default (internal/cli).
	defaultWorkspacesConfigPath = "workspaces/workspace-config.yaml"

	// DefaultPath is where Load looks unless told otherwise.
	DefaultPath = "config.yaml"
)

// The read path's models are fixed rather than configured
// (retrieval-design.md §8). Every latency and quality figure in that design
// was measured against these two, so a swap silently invalidates the numbers
// it rests on — and both slots fail quietly rather than loudly when filled
// wrongly: a non-cross-encoder in the rerank slot reorders badly without
// erroring, and a reasoning model in the LLM slot returns chain-of-thought
// where queries were expected. The embedding model stays configurable by
// contrast because it has a real forcing case: schema_metadata records it and
// the vector dimension must match (ingestion-design.md §4.4).
const (
	RerankModel = "jina-reranker-v3-mlx"
	LLMModel    = "Qwen3.5-4B-MLX-4bit"
)

// RustFS holds the connection and credential settings for Tier 1. Bucket is
// vestigial now that buckets are per-workspace (workspace-isolation.md §2.2)
// — nothing reads it except the root-owned "pocket-advisor" bucket the chart
// still creates, which the pipeline itself no longer uses.
type RustFS struct {
	Endpoint string
	Bucket   string
	UseSSL   bool

	// Root credentials, previously chart-only (values.yaml). Needed here too
	// because the host binary itself now creates and deletes per-workspace
	// buckets and identities (--create-workspace / --delete-workspace,
	// workspace-isolation.md §3, §6) rather than only a one-time Helm-owned
	// setup Job.
	RootAccessKey string
	RootSecretKey string
}

type Postgres struct {
	// AdminDSN is the maintenance connection used only to CREATE/DROP
	// per-workspace databases and roles (workspace-isolation.md §3, §6). It
	// is never used to read or write document data — that always goes
	// through a workspace's own database (Config.WorkspacePostgresDSN).
	AdminDSN string
	// MaxConns must cover every lane that can hold a connection at once. The
	// pgxpool default is max(4, NumCPU), far below the lane count once all
	// roles share a process.
	MaxConns int32
}

type NATS struct {
	URL string
}

// Kubernetes holds what --create-workspace / --delete-workspace need to
// reach the cluster's own API, for the NATS account provisioning step
// (workspace-isolation.md §8). Nothing else in pocket-advisor talks to the
// Kubernetes API — every other interaction is with RustFS, Postgres, or NATS
// over their own protocols.
type Kubernetes struct {
	Namespace       string
	NATSStatefulSet string
	NATSConfigMap   string
}

// Workspace holds the per-workspace secrets --create-workspace provisions
// against. Everything else about a workspace's resources (database name,
// role name, bucket name, identity name, NATS account/user name) is derived
// from its id by convention (workspace-isolation.md §2) — only the
// passwords need to be recorded anywhere.
type Workspace struct {
	PostgresPassword string
	RustFSSecretKey  string
	NATSPassword     string
}

type Embedding struct {
	Endpoint    string
	APIKey      string
	Model       string
	Timeout     time.Duration
	Concurrency int
}

// Reranking and LLM carry only where the model lives. Which model is not
// configurable — see RerankModel / LLMModel and retrieval-design.md §8: every
// latency and quality figure in that design was measured against those two
// specifically, and both slots degrade silently rather than erroring when
// filled wrongly.
type Reranking struct {
	Endpoint string
	APIKey   string
	Timeout  time.Duration
}

// LLM is for query preparation only — decomposition (retrieval-design.md
// §3.6). It is never used for answer generation, which happens outside this
// codebase entirely (§6.1). It is local, fast and already wired up, which is
// exactly what makes that worth stating.
type LLM struct {
	Endpoint string
	APIKey   string
	Timeout  time.Duration
}

// Query is the read-path tuning surface. None of it invalidates the index.
type Query struct {
	VecCandidates        int
	FTSCandidates        int
	RRFK                 int
	DefaultTopK          int
	RerankEnabled        bool
	RerankCandidates     int
	MinRelevanceScore    float64
	MaxPerThread         int
	AnswerContextChars   int
	LexicalDFCeiling     float64
	DecomposeEnabled     bool
	MaxSubQueries        int
	PoolFloorDenseOnly   int
	PoolFloorPerSubQuery int
}

type Config struct {
	RustFS     RustFS
	Postgres   Postgres
	NATS       NATS
	Kubernetes Kubernetes
	Embedding  Embedding
	Reranking  Reranking
	LLM        LLM
	Query      Query

	// WorkspacesConfigPath points at the workspace registry
	// (workspaces/workspace-config.yaml by default) — the same file
	// internal/workspace reads for collections and paths. Per-workspace
	// secrets (workspace-isolation.md §3) live there too, not in this file:
	// that registry is already gitignored (it holds collection paths and,
	// for bank collections, account details), so it is the natural home for
	// credentials as well, while this file stays committed and secret-free.
	WorkspacesConfigPath string

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
			Endpoint      string `yaml:"endpoint"`
			Bucket        string `yaml:"bucket"`
			UseSSL        *bool  `yaml:"use_ssl"`
			RootAccessKey string `yaml:"root_access_key"`
			RootSecretKey string `yaml:"root_secret_key"`
		} `yaml:"rustfs"`
		NATS struct {
			URL string `yaml:"url"`
		} `yaml:"nats"`
		Postgres struct {
			AdminDSN string `yaml:"admin_dsn"`
			MaxConns int32  `yaml:"max_conns"`
		} `yaml:"postgres"`
		Kubernetes struct {
			Namespace       string `yaml:"namespace"`
			NATSStatefulSet string `yaml:"nats_statefulset"`
			NATSConfigMap   string `yaml:"nats_configmap"`
		} `yaml:"kubernetes"`
		Embedding struct {
			Endpoint    string `yaml:"endpoint"`
			Model       string `yaml:"model"`
			Concurrency int    `yaml:"concurrency"`
			Timeout     string `yaml:"timeout"`
		} `yaml:"embedding"`
		Reranking struct {
			Endpoint string `yaml:"endpoint"`
			Timeout  string `yaml:"timeout"`
		} `yaml:"reranking"`
		LLM struct {
			Endpoint string `yaml:"endpoint"`
			Timeout  string `yaml:"timeout"`
		} `yaml:"llm"`
		Observability struct {
			MetricsPort int    `yaml:"metrics_port"`
			LogLevel    string `yaml:"log_level"`
			LogDir      string `yaml:"log_dir"`
		} `yaml:"observability"`
	} `yaml:"infra"`

	// Workspaces is a top-level key, sibling to infra — it only points at
	// the registry file; it does not carry secrets itself (workspace-isolation.md §3).
	Workspaces struct {
		Config string `yaml:"config"`
	} `yaml:"workspaces"`
}

// registryFile mirrors just enough of workspaces/workspace-config.yaml (the
// same file internal/workspace parses more fully for collections and paths)
// to extract one workspace's secrets. Kept deliberately separate from
// internal/workspace's own types — this package only ever needs three
// strings per workspace, not collection resolution.
type registryFile struct {
	Workspaces []struct {
		ID               string `yaml:"id"`
		PostgresPassword string `yaml:"postgres_password"`
		RustFSSecretKey  string `yaml:"rustfs_secret_key"`
		NATSPassword     string `yaml:"nats_password"`
	} `yaml:"workspaces"`
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
		RustFS: RustFS{
			Endpoint:      defaultRustFSEndpoint,
			Bucket:        "pocket-advisor",
			UseSSL:        false,
			RootAccessKey: "rustfsadmin",
			RootSecretKey: "rustfsadminpassword",
		},
		Postgres: Postgres{AdminDSN: defaultPostgresAdminDSN, MaxConns: 50},
		NATS:     NATS{URL: defaultNATSURL},
		Kubernetes: Kubernetes{
			Namespace:       "pocket-advisor",
			NATSStatefulSet: "pocket-advisor-nats",
			NATSConfigMap:   "pocket-advisor-nats-config",
		},
		WorkspacesConfigPath: defaultWorkspacesConfigPath,
		Embedding: Embedding{
			Endpoint:    defaultEmbeddingEndpoint,
			Model:       "jina-embeddings-v5-text-small-mlx",
			Timeout:     60 * time.Second,
			Concurrency: 8,
		},
		Reranking: Reranking{Endpoint: defaultRerankEndpoint, Timeout: 60 * time.Second},
		LLM:       LLM{Endpoint: defaultLLMEndpoint, Timeout: 30 * time.Second},
		Query: Query{
			VecCandidates:        50,
			FTSCandidates:        50,
			RRFK:                 60,
			DefaultTopK:          15,
			RerankEnabled:        true,
			RerankCandidates:     24,
			MinRelevanceScore:    0.0,
			MaxPerThread:         3,
			AnswerContextChars:   120000,
			LexicalDFCeiling:     0.5,
			DecomposeEnabled:     true,
			MaxSubQueries:        4,
			PoolFloorDenseOnly:   6,
			PoolFloorPerSubQuery: 4,
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

	setStr(&c.RustFS.Endpoint, in.RustFS.Endpoint)
	setStr(&c.RustFS.Bucket, in.RustFS.Bucket)
	if in.RustFS.UseSSL != nil {
		c.RustFS.UseSSL = *in.RustFS.UseSSL
	}
	setStr(&c.RustFS.RootAccessKey, in.RustFS.RootAccessKey)
	setStr(&c.RustFS.RootSecretKey, in.RustFS.RootSecretKey)

	setStr(&c.NATS.URL, in.NATS.URL)

	setStr(&c.Postgres.AdminDSN, in.Postgres.AdminDSN)
	if in.Postgres.MaxConns > 0 {
		c.Postgres.MaxConns = in.Postgres.MaxConns
	}

	setStr(&c.Kubernetes.Namespace, in.Kubernetes.Namespace)
	setStr(&c.Kubernetes.NATSStatefulSet, in.Kubernetes.NATSStatefulSet)
	setStr(&c.Kubernetes.NATSConfigMap, in.Kubernetes.NATSConfigMap)

	setStr(&c.WorkspacesConfigPath, f.Workspaces.Config)

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

	setStr(&c.Reranking.Endpoint, in.Reranking.Endpoint)
	if in.Reranking.Timeout != "" {
		d, err := time.ParseDuration(in.Reranking.Timeout)
		if err != nil {
			return fmt.Errorf("%s: infra.reranking.timeout: %w", path, err)
		}
		c.Reranking.Timeout = d
	}

	setStr(&c.LLM.Endpoint, in.LLM.Endpoint)
	if in.LLM.Timeout != "" {
		d, err := time.ParseDuration(in.LLM.Timeout)
		if err != nil {
			return fmt.Errorf("%s: infra.llm.timeout: %w", path, err)
		}
		c.LLM.Timeout = d
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
	c.RustFS.Endpoint = env("RUSTFS_ENDPOINT", c.RustFS.Endpoint)
	c.RustFS.Bucket = env("RUSTFS_BUCKET", c.RustFS.Bucket)
	c.RustFS.UseSSL = envBool("RUSTFS_USE_SSL", c.RustFS.UseSSL)
	c.RustFS.RootAccessKey = env("RUSTFS_ROOT_ACCESS_KEY", c.RustFS.RootAccessKey)
	c.RustFS.RootSecretKey = env("RUSTFS_ROOT_SECRET_KEY", c.RustFS.RootSecretKey)

	c.Reranking.Endpoint = env("RERANK_ENDPOINT", c.Reranking.Endpoint)
	c.Reranking.APIKey = env("RERANK_API_KEY", c.Reranking.APIKey)
	c.LLM.Endpoint = env("LLM_ENDPOINT", c.LLM.Endpoint)
	c.LLM.APIKey = env("LLM_API_KEY", c.LLM.APIKey)

	c.NATS.URL = env("NATS_URL", c.NATS.URL)

	c.Postgres.AdminDSN = env("POSTGRES_ADMIN_DSN", c.Postgres.AdminDSN)
	c.Postgres.MaxConns = int32(envInt("POSTGRES_MAX_CONNS", int(c.Postgres.MaxConns)))

	c.Kubernetes.Namespace = env("KUBERNETES_NAMESPACE", c.Kubernetes.Namespace)
	c.Kubernetes.NATSStatefulSet = env("NATS_STATEFULSET", c.Kubernetes.NATSStatefulSet)
	c.Kubernetes.NATSConfigMap = env("NATS_CONFIGMAP", c.Kubernetes.NATSConfigMap)

	c.WorkspacesConfigPath = env("WORKSPACES_CONFIG", c.WorkspacesConfigPath)

	c.Embedding.Endpoint = env("EMBEDDING_ENDPOINT", c.Embedding.Endpoint)
	c.Embedding.APIKey = env("EMBEDDING_API_KEY", c.Embedding.APIKey)
	c.Embedding.Model = env("EMBEDDING_MODEL", c.Embedding.Model)
	c.Embedding.Timeout = envDuration("EMBEDDING_TIMEOUT", c.Embedding.Timeout)
	c.Embedding.Concurrency = envInt("EMBEDDING_CONCURRENCY", c.Embedding.Concurrency)

	c.MetricsPort = envInt("METRICS_PORT", c.MetricsPort)
	c.LogLevel = env("LOG_LEVEL", c.LogLevel)
	c.LogDir = env("LOG_DIR", c.LogDir)
}

// RequireEmbedding validates the subset the indexer and schema bootstrap need.
func (c *Config) RequireEmbedding() error {
	if c.Embedding.Endpoint == "" {
		return report([]string{"infra.embedding.endpoint"})
	}
	return nil
}

// RequireProvisioning validates what --create-workspace and
// --delete-workspace need beyond a resolved Workspace: the admin/root
// credentials used to create or drop the workspace's own resources
// (workspace-isolation.md §3, §6).
func (c *Config) RequireProvisioning() error {
	var missing []string
	if c.Postgres.AdminDSN == "" {
		missing = append(missing, "infra.postgres.admin_dsn")
	}
	if c.RustFS.RootAccessKey == "" {
		missing = append(missing, "infra.rustfs.root_access_key")
	}
	if c.RustFS.RootSecretKey == "" {
		missing = append(missing, "infra.rustfs.root_secret_key")
	}
	if c.Kubernetes.Namespace == "" {
		missing = append(missing, "infra.kubernetes.namespace")
	}
	if c.Kubernetes.NATSStatefulSet == "" {
		missing = append(missing, "infra.kubernetes.nats_statefulset")
	}
	if c.Kubernetes.NATSConfigMap == "" {
		missing = append(missing, "infra.kubernetes.nats_configmap")
	}
	if c.WorkspacesConfigPath == "" {
		missing = append(missing, "workspaces.config")
	}
	return report(missing)
}

// Workspace resolves a workspace's secrets by id. Everything else about its
// resources (database, role, bucket, identity, NATS account/user names) is
// derived from id by convention (workspace-isolation.md §2), not stored.
func (c *Config) Workspace(id string) (Workspace, error) {
	raw, err := os.ReadFile(c.WorkspacesConfigPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace registry %s: %w", c.WorkspacesConfigPath, err)
	}
	var rf registryFile
	if err := yaml.Unmarshal(raw, &rf); err != nil {
		return Workspace{}, fmt.Errorf("parse workspace registry %s: %w", c.WorkspacesConfigPath, err)
	}
	for _, w := range rf.Workspaces {
		if w.ID == id {
			return Workspace{
				PostgresPassword: w.PostgresPassword,
				RustFSSecretKey:  w.RustFSSecretKey,
				NATSPassword:     w.NATSPassword,
			}, nil
		}
	}
	return Workspace{}, fmt.Errorf("workspace %q has no entry under workspaces: in %s", id, c.WorkspacesConfigPath)
}

// WorkspacePostgresDSN builds the connection string a workspace's own role
// uses, from the admin DSN's host/port and the workspace's own database,
// role, and password (workspace-isolation.md §2.1, §3).
func (c *Config) WorkspacePostgresDSN(id string) (string, error) {
	w, err := c.Workspace(id)
	if err != nil {
		return "", err
	}
	if w.PostgresPassword == "" {
		return "", fmt.Errorf("workspace %q has no postgres_password in %s", id, c.WorkspacesConfigPath)
	}
	u, err := url.Parse(c.Postgres.AdminDSN)
	if err != nil {
		return "", fmt.Errorf("parse infra.postgres.admin_dsn: %w", err)
	}
	u.User = url.UserPassword(id, w.PostgresPassword)
	u.Path = "/" + id
	return u.String(), nil
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
