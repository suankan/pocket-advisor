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
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Cluster Service DNS defaults, matching a `pocket-advisor` release in the
// `pocket-advisor` namespace.
const (
	// One namespace per workspace, and the namespace *is* the workspace id.
	// Every address below is therefore the same service name in a different
	// namespace — hence one %s each, filled with the id.
	defaultRustFSEndpoint = "rustfs-io.%s.svc.cluster.local:9000"
	// Not templated by workspace, unlike RustFS and Postgres: NACK does not
	// deploy NATS, and its model is one server serving Stream CRDs across many
	// namespaces, so there is a single shared server with an account per
	// workspace (deviation 23).
	defaultNATSURL = "nats://nats.pocket-advisor.svc.cluster.local:4222"
	// Each workspace has its own CloudNativePG cluster, and the operator
	// exposes its primary as a Service named <cluster>-rw. The chart names the
	// cluster <release>-<workspace-id>, so this template plus a workspace id
	// is the whole address.
	defaultPostgresHostTemplate = "postgres-rw.%s.svc.cluster.local"
	// The embedding endpoint is a plain localhost address now: the process that
	// calls it runs on the machine that serves it, so the host.docker.internal
	// indirection the in-cluster workers needed is gone.
	defaultEmbeddingEndpoint = "http://localhost:8000/v1/embeddings"
	defaultRerankEndpoint    = "http://localhost:8000/v1/rerank"
	defaultLLMEndpoint       = "http://localhost:8000/v1/chat/completions"

	// Matches the CLI's own --workspace-config default (internal/cli).
	defaultWorkspacesConfigPath = "workspaces/workspace-config.yaml"
	defaultWorkspacesValuesPath = "workspaces/pocket-advisor-infra.yaml"

	// Match charts/pocket-advisor-infra/values.yaml's postgres.appUser and
	// appDatabase: the owner and database CloudNativePG creates in every
	// workspace cluster. Both are the same everywhere — the cluster is already
	// per-workspace, so neither name carries information.
	defaultPostgresAppUser = "app_user"

	// Match charts/pocket-advisor-infra/values.yaml's workspace.name: the bucket, the
	// NATS account and the Postgres database are all called this, in every
	// namespace. The namespace already says whose they are.
	defaultWorkspaceResourceName = "workspace"

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

	// No administrative credentials. Everything this binary does with RustFS
	// it does as a workspace's own identity: the bucket, that identity and its
	// policy are declared by the Tenant CRD, and even the bucket notification
	// rule is within the policy the Tenant grants (deviation 24). The root
	// credentials still exist — the operator needs them — but only the chart
	// ever sees them.
}

type Postgres struct {
	// There is no admin DSN. CloudNativePG gives each workspace its own
	// cluster, so nothing creates a database or a role and there is no
	// maintenance connection to hold (deviation 20). Every connection is a
	// workspace's own, built by WorkspacePostgresDSN.

	// HostTemplate resolves a workspace id to its cluster's primary Service.
	// CNPG names that <cluster>-rw, and the chart names the cluster
	// <release>-<workspace-id>.
	HostTemplate string
	Port         int
	// SSLMode is `require` rather than `verify-full`: CNPG issues its own
	// internal CA, so verifying the chain would mean distributing that CA to
	// the host binary for no gain on a local cluster. `require` still
	// encrypts.
	SSLMode string
	// MaxConns must cover every lane that can hold a connection at once. The
	// pgxpool default is max(4, NumCPU), far below the lane count once all
	// roles share a process.
	MaxConns int32
}

type NATS struct {
	URL string
}

// Workspace is one workspace's infrastructure identity, read from the values
// file that also configures the chart (workspaces.values).
//
// Names are resolved, never empty: each defaults to the workspace id, so
// callers use these fields rather than passing the id around and assuming the
// three systems agree with it.
type Workspace struct {
	ID string
	// Namespace is the workspace id: one release for the whole cluster, but a
	// namespace per workspace. Every address below is the same service name
	// inside it, which is why the names are constants — the namespace already
	// says whose they are (deviations 21, 24).
	Namespace  string
	ValuesPath string

	RustFSEndpoint  string
	BucketName      string
	RustFSAccessKey string
	RustFSSecretKey string

	NATSURL      string
	NATSAccount  string
	NATSUser     string
	NATSPassword string

	PostgresHost string
	DBName       string
	DBUser       string
	DBPassword   string
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
	RustFS    RustFS
	Postgres  Postgres
	NATS      NATS
	Embedding Embedding
	Reranking Reranking
	LLM       LLM
	Query     Query

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
			Endpoint string `yaml:"endpoint"`
			UseSSL   *bool  `yaml:"use_ssl"`
		} `yaml:"rustfs"`
		NATS struct {
			URL string `yaml:"url"`
		} `yaml:"nats"`
		Postgres struct {
			HostTemplate string `yaml:"host_template"`
			Port         int    `yaml:"port"`
			SSLMode      string `yaml:"sslmode"`
			MaxConns     int32  `yaml:"max_conns"`
		} `yaml:"postgres"`
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

// valuesFile mirrors workspaces/pocket-advisor-infra.yaml — the private
// override Helm is given with -f, so one file configures both the chart and
// this binary and the two cannot disagree about a password.
//
// Credentials only. Every resource name is a constant now: one release per
// namespace means the namespace identifies the workspace, so a name derived
// from the id would only repeat it (deviation 21).
//
// Read per call rather than at Load: there is no longer one values file to
// resolve at startup, and a mode that never touches a workspace should not
// fail because some workspace's file is missing.
//
// Deliberately separate from internal/workspace's types, which parse the
// corpus side (collections, paths) out of workspace-config.yaml. The two files
// are joined on id: infrastructure here, content there.
type valuesFile struct {
	Workspaces []struct {
		ID     string `yaml:"id"`
		RustFS struct {
			Credentials struct {
				SecretKey string `yaml:"secretKey"`
			} `yaml:"credentials"`
		} `yaml:"rustfs"`
		NATS struct {
			Credentials struct {
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"nats"`
		Postgres struct {
			Credentials struct {
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"postgres"`
	} `yaml:"workspaces"`
}

func defaults() *Config {
	return &Config{
		RustFS: RustFS{
			Endpoint: defaultRustFSEndpoint,
			UseSSL:   false,
		},
		Postgres: Postgres{
			HostTemplate: defaultPostgresHostTemplate,
			Port:         5432,
			SSLMode:      "require",
			MaxConns:     50,
		},
		NATS:                 NATS{URL: defaultNATSURL},
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

	setStr(&c.NATS.URL, in.NATS.URL)

	setStr(&c.Postgres.HostTemplate, in.Postgres.HostTemplate)
	setStr(&c.Postgres.SSLMode, in.Postgres.SSLMode)
	if in.Postgres.Port != 0 {
		c.Postgres.Port = in.Postgres.Port
	}
	if in.Postgres.MaxConns > 0 {
		c.Postgres.MaxConns = in.Postgres.MaxConns
	}

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

	c.Reranking.Endpoint = env("RERANK_ENDPOINT", c.Reranking.Endpoint)
	c.Reranking.APIKey = env("RERANK_API_KEY", c.Reranking.APIKey)
	c.LLM.Endpoint = env("LLM_ENDPOINT", c.LLM.Endpoint)
	c.LLM.APIKey = env("LLM_API_KEY", c.LLM.APIKey)

	c.NATS.URL = env("NATS_URL", c.NATS.URL)

	c.Postgres.HostTemplate = env("POSTGRES_HOST_TEMPLATE", c.Postgres.HostTemplate)
	c.Postgres.MaxConns = int32(envInt("POSTGRES_MAX_CONNS", int(c.Postgres.MaxConns)))

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

// Workspace resolves a workspace's secrets by id, and returns every address
// and name already resolved so callers never derive one themselves. Only the
// NATS account is named after the id; the bucket, database and owner are
// constants, because a namespace holds one of each (workspace-isolation.md §2).
func (c *Config) Workspace(id string) (Workspace, error) {
	if id == "" {
		return Workspace{}, fmt.Errorf("workspace id is required")
	}
	path := c.WorkspacesValuesPath
	raw, err := os.ReadFile(path)
	if err != nil {
		return Workspace{}, fmt.Errorf("read workspace values %s: %w", path, err)
	}
	var vf valuesFile
	if err := yaml.Unmarshal(raw, &vf); err != nil {
		return Workspace{}, fmt.Errorf("parse workspace values %s: %w", path, err)
	}

	var entry *struct {
		ID     string `yaml:"id"`
		RustFS struct {
			Credentials struct {
				SecretKey string `yaml:"secretKey"`
			} `yaml:"credentials"`
		} `yaml:"rustfs"`
		NATS struct {
			Credentials struct {
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"nats"`
		Postgres struct {
			Credentials struct {
				Password string `yaml:"password"`
			} `yaml:"credentials"`
		} `yaml:"postgres"`
	}
	for i := range vf.Workspaces {
		if vf.Workspaces[i].ID == id {
			entry = &vf.Workspaces[i]
			break
		}
	}
	if entry == nil {
		return Workspace{}, fmt.Errorf("workspace %q has no entry under workspaces: in %s", id, path)
	}
	for field, v := range map[string]string{
		"rustfs.credentials.secretKey":  entry.RustFS.Credentials.SecretKey,
		"nats.credentials.password":     entry.NATS.Credentials.Password,
		"postgres.credentials.password": entry.Postgres.Credentials.Password,
	} {
		if v == "" {
			return Workspace{}, fmt.Errorf("workspace %q: %s is empty in %s", id, field, path)
		}
	}

	// The namespace is the workspace id, and every address is the same service
	// name inside it. Nothing here is looked up by name in a shared namespace
	// any more, which is why the resource names are all one constant.
	return Workspace{
		ID:         id,
		Namespace:  id,
		ValuesPath: path,

		RustFSEndpoint:  fmt.Sprintf(c.RustFS.Endpoint, id),
		BucketName:      defaultWorkspaceResourceName,
		RustFSAccessKey: defaultWorkspaceResourceName,
		RustFSSecretKey: entry.RustFS.Credentials.SecretKey,

		NATSURL: c.NATS.URL,
		// Keyed by workspace id, unlike the bucket and database: those are
		// alone in a namespace, while accounts share one server.
		NATSAccount:  id,
		NATSUser:     id,
		NATSPassword: entry.NATS.Credentials.Password,

		PostgresHost: fmt.Sprintf(c.Postgres.HostTemplate, id),
		DBName:       defaultWorkspaceResourceName,
		DBUser:       defaultPostgresAppUser,
		DBPassword:   entry.Postgres.Credentials.Password,
	}, nil
}

// WorkspacePostgresDSN builds the connection string a workspace's own role
// uses, from its cluster's primary Service and its own database, owner and
// password (workspace-isolation.md §2.1, §3). It is the only Postgres
// connection string in this project: there is no administrative one.
func (c *Config) WorkspacePostgresDSN(id string) (string, error) {
	w, err := c.Workspace(id)
	if err != nil {
		return "", err
	}
	if w.DBPassword == "" {
		return "", fmt.Errorf("workspace %q has no postgres.credentials.password in %s", id, c.WorkspacesValuesPath)
	}
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(w.DBUser, w.DBPassword),
		Host:     net.JoinHostPort(w.PostgresHost, strconv.Itoa(c.Postgres.Port)),
		Path:     "/" + w.DBName,
		RawQuery: "sslmode=" + c.Postgres.SSLMode,
	}).String(), nil
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
