// Package config resolves service configuration.
//
// config.yaml is the only source of truth for infrastructure — there is no
// Go-side fallback for a store address, an endpoint, or a fixed model name
// any more. This package layers only the `infra` section of config.yaml,
// then the environment, over nothing: an address this package used to
// default to a stock local cluster now has to come from the file or the
// environment, or Load reports it missing rather than silently guessing.
// Flags override all of it at the call site. The one path that IS hardcoded
// is DefaultPath itself — Load has to know where to look before it can read
// anything that would tell it otherwise.
//
// The app runs on the host while its stores stay in the local OrbStack
// cluster, so config.yaml's own committed values are the cluster's Service
// DNS names. OrbStack routes *.svc.cluster.local from macOS, but only
// through the system resolver — which Go consults only when cgo is enabled.
// mise.toml pins CGO_ENABLED=1 for that as much as for Tesseract.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where Load looks unless told otherwise. The only
// infrastructure value left hardcoded in Go — Load has to know where
// config.yaml is before it can read the rest from it.
const DefaultPath = "config.yaml"

var environmentPlaceholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvironmentPlaceholders resolves the ${NAME} placeholders permitted
// in the committed config template. os.ExpandEnv would silently turn an
// unset one into an empty string; reporting it here keeps a missing
// environment variable from becoming a confusing downstream failure instead.
func expandEnvironmentPlaceholders(value, field string) (string, error) {
	var missing []string
	expanded := environmentPlaceholder.ReplaceAllStringFunc(value, func(match string) string {
		name := match[2 : len(match)-1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return value
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("expand %s: unset environment variable(s): %s", field, strings.Join(missing, ", "))
	}
	return expanded, nil
}

func expandFilePlaceholders(f *file) error {
	fields := map[string]*string{
		"infra.rustfs.endpoint":         &f.Infra.RustFS.Endpoint,
		"infra.nats.url":                &f.Infra.NATS.URL,
		"infra.postgres.host":           &f.Infra.Postgres.Host,
		"infra.postgres.sslmode":        &f.Infra.Postgres.SSLMode,
		"infra.embedding.endpoint":      &f.Infra.Embedding.Endpoint,
		"infra.embedding.model":         &f.Infra.Embedding.Model,
		"infra.embedding.timeout":       &f.Infra.Embedding.Timeout,
		"infra.reranking.endpoint":      &f.Infra.Reranking.Endpoint,
		"infra.reranking.model":         &f.Infra.Reranking.Model,
		"infra.reranking.timeout":       &f.Infra.Reranking.Timeout,
		"infra.llm.endpoint":            &f.Infra.LLM.Endpoint,
		"infra.llm.model":               &f.Infra.LLM.Model,
		"infra.llm.timeout":             &f.Infra.LLM.Timeout,
		"infra.observability.log_dir":   &f.Infra.Observability.LogDir,
		"infra.observability.log_level": &f.Infra.Observability.LogLevel,
		"workspaces.config":             &f.Workspaces.Config,
	}
	for field, value := range fields {
		expanded, err := expandEnvironmentPlaceholders(*value, field)
		if err != nil {
			return err
		}
		*value = expanded
	}
	return nil
}

// RustFS holds the connection and credential settings for Tier 1. There is no
// bucket here: every bucket is a workspace's own (workspace-isolation.md
// §2.2), and the shared one this used to name was deleted along with the setup
// Job that created it (ingestion-design.md deviation 19).
type RustFS struct {
	Endpoint string
	UseSSL   bool

	// No administrative credentials, and no per-workspace ones either: the
	// bucket, its public policy, and its notification binding are all
	// created once by `./pocket-advisor.sh deploy-workspaces`, over aws-cli,
	// not by anything this binary runs (deviation 39). This binary connects
	// to a workspace's bucket anonymously — isolation is the bucket name and
	// its policy, not a credential. The RustFS root identity still exists
	// (deploy-workspaces/destroy-workspace need it to create buckets and
	// policies) but is the fixed literal admin/admin
	// (charts/pocket-advisor-infra/values.yaml); nothing about it lives in
	// Go.
}

type Postgres struct {
	// There is no admin DSN. Every workspace's database and role are
	// declared by the chart (Database/DatabaseRole CRDs, deviation 34), so
	// nothing this binary does creates one, and there is no maintenance
	// connection to hold. Every connection is a workspace's own, built by
	// WorkspacePostgresDSN.

	// Host is the shared Postgres StatefulSet's Service. Not templated by
	// workspace: there is one address, in the release namespace, the same as
	// NATS.URL's.
	Host string
	Port int
	// SSLMode is `disable`, not `require`. CloudNativePG issued its own
	// internal CA and terminated TLS for free; the plain StatefulSet that
	// replaced it (deviation 39) does none of that — no cert, no key, no
	// `ssl = on` in postgresql.conf — so `require` would refuse every
	// connection outright rather than degrade to unencrypted, which is a
	// worse failure than the one it would have been guarding against on a
	// single-machine local cluster with no network segment to eavesdrop on.
	// Encrypting this would mean provisioning and rotating a cert by hand,
	// the kind of machinery deviation 39 removed operators specifically to
	// avoid — revisit only if this cluster stops being local and single-user.
	SSLMode string
	// MaxConns must cover every lane that can hold a connection at once. The
	// pgxpool default is max(4, NumCPU), far below the lane count once all
	// roles share a process.
	MaxConns int32
}

type NATS struct {
	URL string
}

// MCP holds the MCP server configuration for local execution.
type MCP struct {
	HTTP  MCPHTTP
	OAuth MCPOAuth
	TLS   MCPTLS
}

// MCPHTTP holds HTTP server settings.
type MCPHTTP struct {
	Addr          string
	Endpoint      string
	ResourceURI   string
	MaxConcurrent int
}

// MCPOAuth holds Google-IDP settings for authenticated HTTP mode. Leaving
// GoogleClientID empty runs the HTTP server unauthenticated on loopback
// (local development); setting it requires AllowedEmails to be non-empty.
type MCPOAuth struct {
	GoogleClientID string
	AllowedEmails  []string
}

// MCPTLS holds TLS configuration for local HTTPS.
type MCPTLS struct {
	CertFile string
	KeyFile  string
}

// Workspace is one workspace's infrastructure identity. Every name and
// credential is the workspace id itself, or derived from it by fixed
// convention — nothing per-workspace is stored anywhere, so this is a pure
// function of id, never a file read.
//
// Postgres keeps a real per-workspace role (trust auth skips the password
// check, not Postgres's own privilege checks, so a role owning only its own
// database is still an enforced boundary). RustFS and NATS have no
// per-workspace identity at all any more: RustFS isolation is a public
// bucket policy scoped to that bucket's name, and NATS isolation is subject/
// stream naming inside internal/bus — both are convention, not credentials.
type Workspace struct {
	ID string

	RustFSEndpoint string
	BucketName     string

	NATSURL string

	PostgresHost string
	DBName       string
	DBUser       string
}

type Embedding struct {
	Endpoint    string
	APIKey      string
	Model       string
	Timeout     time.Duration
	Concurrency int
}

// Reranking's Model is config.yaml's infra.reranking.model, not a Go
// constant — see the comment on that key for why changing it is a measured
// decision, not a free knob, even though nothing in code enforces that any
// more (retrieval-design.md §8).
type Reranking struct {
	Endpoint string
	APIKey   string
	Model    string
	Timeout  time.Duration
}

// LLM is for query preparation only — decomposition (retrieval-design.md
// §3.6). It is never used for answer generation, which happens outside this
// codebase entirely (§6.1). It is local, fast and already wired up, which is
// exactly what makes that worth stating. Model is config.yaml's
// infra.llm.model, the same non-obvious-but-not-enforced contract as
// Reranking.Model above.
type LLM struct {
	Endpoint string
	APIKey   string
	Model    string
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
	AnswerContextBytes   int
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
	MCP       MCP

	// WorkspacesConfigPath points at the workspace registry
	// (config.yaml's workspaces.config — required, no fallback) — the same
	// file internal/workspace reads for collections and paths. There is no
	// separate credentials file any more: every workspace's Postgres role,
	// RustFS bucket, and NATS subjects/streams are named after its id by
	// fixed convention (see Workspace), so nothing per-workspace needs its
	// own registry entry.
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
			Endpoint string `yaml:"endpoint"`
			UseSSL   *bool  `yaml:"use_ssl"`
		} `yaml:"rustfs"`
		NATS struct {
			URL string `yaml:"url"`
		} `yaml:"nats"`
		Postgres struct {
			Host     string `yaml:"host"`
			Port     int    `yaml:"port"`
			SSLMode  string `yaml:"sslmode"`
			MaxConns int32  `yaml:"max_conns"`
		} `yaml:"postgres"`
		Embedding struct {
			Endpoint    string `yaml:"endpoint"`
			Model       string `yaml:"model"`
			Concurrency int    `yaml:"concurrency"`
			Timeout     string `yaml:"timeout"`
		} `yaml:"embedding"`
		Reranking struct {
			Endpoint string `yaml:"endpoint"`
			Model    string `yaml:"model"`
			Timeout  string `yaml:"timeout"`
		} `yaml:"reranking"`
		LLM struct {
			Endpoint string `yaml:"endpoint"`
			Model    string `yaml:"model"`
			Timeout  string `yaml:"timeout"`
		} `yaml:"llm"`
		Observability struct {
			MetricsPort int    `yaml:"metrics_port"`
			LogLevel    string `yaml:"log_level"`
			LogDir      string `yaml:"log_dir"`
		} `yaml:"observability"`
	} `yaml:"infra"`

	MCP struct {
		HTTP struct {
			Addr          string `yaml:"addr"`
			Endpoint      string `yaml:"endpoint"`
			ResourceURI   string `yaml:"resource_uri"`
			MaxConcurrent int    `yaml:"max_concurrent"`
		} `yaml:"http"`
		OAuth struct {
			GoogleClientID string   `yaml:"google_client_id"`
			AllowedEmails  []string `yaml:"allowed_emails"`
		} `yaml:"oauth"`
		TLS struct {
			CertFile string `yaml:"cert_file"`
			KeyFile  string `yaml:"key_file"`
		} `yaml:"tls"`
	} `yaml:"mcp"`

	// Workspaces is a top-level key, sibling to infra — it only points at
	// the registry file; it does not carry secrets itself (workspace-isolation.md §3).
	Workspaces struct {
		Config string `yaml:"config"`
	} `yaml:"workspaces"`
}

// Load resolves configuration from defaults, then path, then the environment.
//
// A missing file is now an error, not a fallback to a stock local cluster:
// there is no Go-side default left for any of the fields requireInfra
// checks, so a missing config.yaml leaves them all empty rather than filled
// in from somewhere else. A malformed file is an error for the same
// long-standing reason — silently ingesting into the wrong store is worse
// than refusing to start.
func Load(path string) (*Config, error) {
	c := defaults()
	if err := applyFile(c, path); err != nil {
		return nil, err
	}
	applyEnv(c)
	if err := requireInfra(c); err != nil {
		return nil, err
	}
	return c, nil
}

// requireInfra reports every field that used to have a Go-side default and
// no longer does — config.yaml (or the environment) is the only place left
// that can supply them, so one left empty after applyFile and applyEnv is a
// configuration error, not a silent gap. Embedding.Endpoint is deliberately
// not here: RequireEmbedding already covers it, called only by the modes
// that actually need it, and duplicating that check here would just be two
// sources of truth for the same field.
func requireInfra(c *Config) error {
	var missing []string
	if c.RustFS.Endpoint == "" {
		missing = append(missing, "infra.rustfs.endpoint")
	}
	if c.NATS.URL == "" {
		missing = append(missing, "infra.nats.url")
	}
	if c.Postgres.Host == "" {
		missing = append(missing, "infra.postgres.host")
	}
	if c.Reranking.Endpoint == "" {
		missing = append(missing, "infra.reranking.endpoint")
	}
	if c.Reranking.Model == "" {
		missing = append(missing, "infra.reranking.model")
	}
	if c.LLM.Endpoint == "" {
		missing = append(missing, "infra.llm.endpoint")
	}
	if c.LLM.Model == "" {
		missing = append(missing, "infra.llm.model")
	}
	if c.WorkspacesConfigPath == "" {
		missing = append(missing, "workspaces.config")
	}
	return report(missing)
}

// defaults covers only the fields that still have one — every field
// requireInfra checks starts at its zero value here on purpose, since
// config.yaml (or the environment) is now the only place any of them can
// come from.
func defaults() *Config {
	return &Config{
		RustFS: RustFS{
			UseSSL: false,
		},
		Postgres: Postgres{
			Port:     5432,
			SSLMode:  "disable",
			MaxConns: 50,
		},
		Embedding: Embedding{
			Model:       "jina-embeddings-v5-text-small-mlx",
			Timeout:     60 * time.Second,
			Concurrency: 8,
		},
		Reranking: Reranking{Timeout: 60 * time.Second},
		LLM:       LLM{Timeout: 30 * time.Second},
		Query: Query{
			VecCandidates:        50,
			FTSCandidates:        50,
			RRFK:                 60,
			DefaultTopK:          15,
			RerankEnabled:        true,
			RerankCandidates:     24,
			MinRelevanceScore:    0.0,
			MaxPerThread:         3,
			AnswerContextBytes:   120000,
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
	if err := expandFilePlaceholders(&f); err != nil {
		return fmt.Errorf("expand %s: %w", path, err)
	}
	in := f.Infra

	setStr(&c.RustFS.Endpoint, in.RustFS.Endpoint)
	if in.RustFS.UseSSL != nil {
		c.RustFS.UseSSL = *in.RustFS.UseSSL
	}

	setStr(&c.NATS.URL, in.NATS.URL)

	setStr(&c.Postgres.Host, in.Postgres.Host)
	setStr(&c.Postgres.SSLMode, in.Postgres.SSLMode)
	if in.Postgres.Port != 0 {
		c.Postgres.Port = in.Postgres.Port
	}
	if in.Postgres.MaxConns > 0 {
		c.Postgres.MaxConns = in.Postgres.MaxConns
	}

	setStr(&c.WorkspacesConfigPath, f.Workspaces.Config)

	// The workspace registry path is relative to the config file that
	// declares it, never to the process's working directory: it points at a
	// file sitting beside config.yaml in the repository, so the directory it
	// was read from is the only anchor that means anything.
	//
	// Resolving it against the cwd instead is invisible when the process runs
	// from the repository root and fatal when it does not — which is exactly
	// how an MCP client launches this binary. --workspace-config was added to
	// work around that.
	//
	// Applied unconditionally, even when the file didn't set it (it stays ""
	// and requireInfra reports it missing below) — resolveAgainst itself is a
	// no-op on an empty string, so there is nothing to guard here. A bare
	// "config.yaml" gives a directory of ".", which leaves a path the file
	// did set exactly as written.
	dir := filepath.Dir(path)
	c.WorkspacesConfigPath = resolveAgainst(dir, c.WorkspacesConfigPath)

	// Same reasoning as the two workspace paths above: LogDir defaults to the
	// relative "logs" and must anchor to config.yaml's directory, not the
	// process's cwd, or an MCP client launching the binary from an arbitrary
	// directory gets a log directory (and, for `mcp start`/`mcp stop`, a PID
	// file location) that silently drifts with whatever cwd it happened to
	// launch from.
	c.LogDir = resolveAgainst(dir, c.LogDir)

	// MCP configuration
	setStr(&c.MCP.HTTP.Addr, f.MCP.HTTP.Addr)
	setStr(&c.MCP.HTTP.Endpoint, f.MCP.HTTP.Endpoint)
	setStr(&c.MCP.HTTP.ResourceURI, f.MCP.HTTP.ResourceURI)
	if f.MCP.HTTP.MaxConcurrent > 0 {
		c.MCP.HTTP.MaxConcurrent = f.MCP.HTTP.MaxConcurrent
	}
	setStr(&c.MCP.OAuth.GoogleClientID, f.MCP.OAuth.GoogleClientID)
	if len(f.MCP.OAuth.AllowedEmails) > 0 {
		c.MCP.OAuth.AllowedEmails = f.MCP.OAuth.AllowedEmails
	}
	setStr(&c.MCP.TLS.CertFile, f.MCP.TLS.CertFile)
	setStr(&c.MCP.TLS.KeyFile, f.MCP.TLS.KeyFile)
	// Same reasoning as LogDir/WorkspacesConfigPath above: a client launching
	// `mcp start --config <path>` from a directory other than the repo root
	// needs these anchored to where config.yaml lives, not to its own cwd.
	c.MCP.TLS.CertFile = resolveAgainst(dir, c.MCP.TLS.CertFile)
	c.MCP.TLS.KeyFile = resolveAgainst(dir, c.MCP.TLS.KeyFile)

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
	setStr(&c.Reranking.Model, in.Reranking.Model)
	if in.Reranking.Timeout != "" {
		d, err := time.ParseDuration(in.Reranking.Timeout)
		if err != nil {
			return fmt.Errorf("%s: infra.reranking.timeout: %w", path, err)
		}
		c.Reranking.Timeout = d
	}

	setStr(&c.LLM.Endpoint, in.LLM.Endpoint)
	setStr(&c.LLM.Model, in.LLM.Model)
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
	c.Reranking.Model = env("RERANK_MODEL", c.Reranking.Model)
	c.LLM.Endpoint = env("LLM_ENDPOINT", c.LLM.Endpoint)
	c.LLM.APIKey = env("LLM_API_KEY", c.LLM.APIKey)
	c.LLM.Model = env("LLM_MODEL", c.LLM.Model)

	c.NATS.URL = env("NATS_URL", c.NATS.URL)

	c.Postgres.Host = env("POSTGRES_HOST", c.Postgres.Host)
	c.Postgres.MaxConns = int32(envInt("POSTGRES_MAX_CONNS", int(c.Postgres.MaxConns)))

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

// Workspace resolves a workspace's infrastructure identity by id. Every
// name is the id itself, or derived from it by fixed convention — this is a
// pure function, never a file read, since nothing is stored per workspace
// any more (see Workspace's own doc comment for what still is, and is not,
// an enforced boundary per store).
func (c *Config) Workspace(id string) (Workspace, error) {
	if id == "" {
		return Workspace{}, fmt.Errorf("workspace id is required")
	}
	return Workspace{
		ID: id,

		// One shared RustFS StatefulSet (deviation 39, replacing deviation
		// 35's Tenant CRD): the endpoint is constant, and the bucket name is
		// what `./pocket-advisor.sh deploy-workspaces` actually created via
		// aws-cli, not a CRD. No identity: the bucket carries a public
		// policy scoped to itself, and the application connects anonymously.
		RustFSEndpoint: c.RustFS.Endpoint,
		BucketName:     id,

		NATSURL: c.NATS.URL,

		// One shared Postgres StatefulSet (deviation 39, replacing deviation
		// 34's Cluster CRD): the host is constant, and the database/role
		// names are what `./pocket-advisor.sh deploy-workspaces` actually
		// created via psql, not a Database/DatabaseRole CRD. No password:
		// pg_hba.conf trusts every connection (charts/pocket-advisor-infra/
		// templates/postgres.yaml), so the role name alone is what
		// Postgres's own privilege checks key off of.
		PostgresHost: c.Postgres.Host,
		DBName:       id,
		DBUser:       id,
	}, nil
}

// WorkspacePostgresDSN builds the connection string a workspace's own role
// uses, from the shared cluster's primary Service and its own database,
// owner and password (workspace-isolation.md §2.1, §3). It is the only
// Postgres connection string in this project: there is no administrative
// one.
func (c *Config) WorkspacePostgresDSN(id string) (string, error) {
	w, err := c.Workspace(id)
	if err != nil {
		return "", err
	}
	// No password: pg_hba.conf trusts every connection, so the role name
	// alone is the credential.
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.User(w.DBUser),
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

// resolveAgainst anchors a relative path to dir. An absolute path is already
// unambiguous and is left exactly as given, which is what lets an operator
// point at a registry outside the repository.
func resolveAgainst(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
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
