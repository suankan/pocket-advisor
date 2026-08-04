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
	//
	// Carries no user or password: those are injected from
	// workspaces/values.yaml by applyWorkspaceValues, so no credential is
	// committed here or in config.yaml.
	defaultPostgresAdminDSN = "postgres://" +
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
	defaultWorkspacesValuesPath = "workspaces/values.yaml"

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

// RustFS holds the connection and credential settings for Tier 1. There is no
// bucket here: every bucket is a workspace's own (workspace-isolation.md
// §2.2), and the shared one this used to name was deleted along with the setup
// Job that created it (ingestion-design.md deviation 19).
type RustFS struct {
	Endpoint string
	UseSSL   bool

	// Administrative credentials, read from workspaces/values.yaml — the same
	// file Helm is given — rather than from committed config. Used only to
	// create and delete a workspace's own bucket and identity
	// (--create-workspace / --delete-workspace, workspace-isolation.md §3,
	// §6); never to read or write documents.
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

// Kubernetes holds what --create-workspace / --delete-workspace need to reach
// the cluster's own API: writing the RustFS notify identity as a Secret and
// restarting RustFS to pick it up (§5.2). It no longer covers NATS account
// provisioning — the chart renders those (deviation 18). Nothing else in
// pocket-advisor talks to the Kubernetes API.
type Kubernetes struct {
	Namespace       string
	NATSStatefulSet string
	NATSConfigMap   string
}

// Workspace is one workspace's infrastructure identity, read from the values
// file that also configures the chart (workspaces.values).
//
// Names are resolved, never empty: each defaults to the workspace id, so
// callers use these fields rather than passing the id around and assuming the
// three systems agree with it.
type Workspace struct {
	DBName     string
	DBUser     string
	DBPassword string

	BucketName      string
	RustFSAccessKey string
	RustFSSecretKey string

	NATSAccount  string
	NATSUser     string
	NATSPassword string
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
	// internal/workspace reads for collections and paths. It describes what a
	// workspace *holds*; credentials moved out of it entirely and now live in
	// WorkspacesValuesPath below (deviation 18).
	WorkspacesConfigPath string

	// WorkspacesValuesPath points at the private Helm values override that
	// carries each workspace's infrastructure names and credentials. The same
	// file `make deploy-infra` passes to helm with -f, so the chart and this
	// binary cannot disagree about a password.
	WorkspacesValuesPath string

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
		Values string `yaml:"values"`
	} `yaml:"workspaces"`
}

// valuesFile mirrors workspaces/values.yaml — the private override Helm is
// given with -f, parsed here so one file configures both the chart and this
// binary and the two cannot disagree about a password.
//
// Deliberately separate from internal/workspace's types, which parse the
// corpus side (collections, paths) out of workspace-config.yaml. The two files
// are joined on id: infrastructure here, content there.
type valuesFile struct {
	RustFS struct {
		Credentials struct {
			RootUser     string `yaml:"rootUser"`
			RootPassword string `yaml:"rootPassword"`
		} `yaml:"credentials"`
	} `yaml:"rustfs"`
	Postgres struct {
		Credentials struct {
			User     string `yaml:"user"`
			Password string `yaml:"password"`
		} `yaml:"credentials"`
	} `yaml:"postgres"`
	// Each entry mirrors the root sections above: same names, same credentials
	// nesting. `rustfs` means the same thing at both levels, and only the
	// scope differs — administrative there, one workspace's own here.
	Workspaces []struct {
		ID     string `yaml:"id"`
		RustFS struct {
			Bucket      string `yaml:"bucket"`
			Credentials struct {
				AccessKey string `yaml:"accessKey"`
				SecretKey string `yaml:"secretKey"`
			} `yaml:"credentials"`
		} `yaml:"rustfs"`
		Postgres struct {
			Database    string `yaml:"database"`
			Credentials struct {
				User     string `yaml:"user"`
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"postgres"`
		NATS struct {
			Account     string `yaml:"account"`
			Credentials struct {
				User     string `yaml:"user"`
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"nats"`
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
	// Credentials come from the values file, not from here: config.yaml is
	// committed, and a committed file must not carry a password. Applied
	// before applyEnv so an environment variable still wins, which is what
	// CI and a second cluster need.
	if err := applyWorkspaceValues(c); err != nil {
		return nil, err
	}
	applyEnv(c)
	return c, nil
}

// applyWorkspaceValues fills in the administrative credentials from
// workspaces/values.yaml — the same file Helm is given, so the cluster and
// this binary use one definition of "the admin password" rather than two that
// can drift.
//
// A missing file is not an error. Read-path modes (--query, --mcp) need no
// admin credentials at all, and failing to start over an absent file would
// make them depend on provisioning config they never use.
func applyWorkspaceValues(c *Config) error {
	if c.WorkspacesValuesPath == "" {
		return nil
	}
	raw, err := os.ReadFile(c.WorkspacesValuesPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workspace values %s: %w", c.WorkspacesValuesPath, err)
	}
	var vf valuesFile
	if err := yaml.Unmarshal(raw, &vf); err != nil {
		return fmt.Errorf("parse workspace values %s: %w", c.WorkspacesValuesPath, err)
	}

	setStr(&c.RustFS.RootAccessKey, vf.RustFS.Credentials.RootUser)
	setStr(&c.RustFS.RootSecretKey, vf.RustFS.Credentials.RootPassword)

	// The admin DSN is committed without credentials; they are injected here.
	if u := vf.Postgres.Credentials.User; u != "" && c.Postgres.AdminDSN != "" {
		parsed, err := url.Parse(c.Postgres.AdminDSN)
		if err != nil {
			return fmt.Errorf("parse infra.postgres.admin_dsn: %w", err)
		}
		parsed.User = url.UserPassword(u, vf.Postgres.Credentials.Password)
		c.Postgres.AdminDSN = parsed.String()
	}
	return nil
}

func defaults() *Config {
	return &Config{
		RustFS: RustFS{
			Endpoint: defaultRustFSEndpoint,
			UseSSL:   false,
		},
		Postgres: Postgres{AdminDSN: defaultPostgresAdminDSN, MaxConns: 50},
		NATS:     NATS{URL: defaultNATSURL},
		Kubernetes: Kubernetes{
			Namespace:       "pocket-advisor",
			NATSStatefulSet: "pocket-advisor-nats",
			NATSConfigMap:   "pocket-advisor-nats-config",
		},
		WorkspacesConfigPath: defaultWorkspacesConfigPath,
		WorkspacesValuesPath: defaultWorkspacesValuesPath,
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
	setStr(&c.WorkspacesValuesPath, f.Workspaces.Values)

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
	c.WorkspacesValuesPath = env("WORKSPACES_VALUES", c.WorkspacesValuesPath)

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
	if c.WorkspacesValuesPath == "" {
		missing = append(missing, "workspaces.values")
	}
	return report(missing)
}

// Workspace resolves a workspace's secrets by id. Everything else about its
// resources (database, role, bucket, identity, NATS account/user names) is
// derived from id by convention (workspace-isolation.md §2), not stored.
func (c *Config) Workspace(id string) (Workspace, error) {
	if c.WorkspacesValuesPath == "" {
		return Workspace{}, fmt.Errorf("workspaces.values is not set in config.yaml")
	}
	raw, err := os.ReadFile(c.WorkspacesValuesPath)
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace values %s: %w", c.WorkspacesValuesPath, err)
	}
	var vf valuesFile
	if err := yaml.Unmarshal(raw, &vf); err != nil {
		return Workspace{}, fmt.Errorf("parse workspace values %s: %w", c.WorkspacesValuesPath, err)
	}
	for _, w := range vf.Workspaces {
		if w.ID != id {
			continue
		}
		or := func(v, fallback string) string {
			if v == "" {
				return fallback
			}
			return v
		}
		return Workspace{
			DBName:     or(w.Postgres.Database, id),
			DBUser:     or(w.Postgres.Credentials.User, id),
			DBPassword: w.Postgres.Credentials.Password,

			BucketName:      or(w.RustFS.Bucket, id),
			RustFSAccessKey: or(w.RustFS.Credentials.AccessKey, id),
			RustFSSecretKey: w.RustFS.Credentials.SecretKey,

			NATSAccount:  or(w.NATS.Account, id),
			NATSUser:     or(w.NATS.Credentials.User, id),
			NATSPassword: w.NATS.Credentials.Password,
		}, nil
	}
	return Workspace{}, fmt.Errorf("workspace %q has no entry under workspaces: in %s", id, c.WorkspacesValuesPath)
}

// WorkspacePostgresDSN builds the connection string a workspace's own role
// uses, from the admin DSN's host/port and the workspace's own database,
// role, and password (workspace-isolation.md §2.1, §3).
func (c *Config) WorkspacePostgresDSN(id string) (string, error) {
	w, err := c.Workspace(id)
	if err != nil {
		return "", err
	}
	if w.DBPassword == "" {
		return "", fmt.Errorf("workspace %q has no postgres.credentials.password in %s", id, c.WorkspacesValuesPath)
	}
	u, err := url.Parse(c.Postgres.AdminDSN)
	if err != nil {
		return "", fmt.Errorf("parse infra.postgres.admin_dsn: %w", err)
	}
	u.User = url.UserPassword(w.DBUser, w.DBPassword)
	u.Path = "/" + w.DBName
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
