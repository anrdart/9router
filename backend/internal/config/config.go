// Package config loads 9router Go-backend configuration from environment.
//
// It mirrors the env contract of the existing Node app (.env.example) so the Go
// backend can run alongside it with a single shared .env file. Only the subset of
// variables relevant to the Go reverse-proxy bridge is modeled here; unknown
// variables are passed through unchanged to the Node upstream via the process env.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all Go-backend runtime knobs.
type Config struct {
	// Host is the interface the Go backend binds to. Defaults to loopback
	// (127.0.0.1) so the dashboard/native API is unreachable from the network
	// unless explicitly exposed via GO_HOST/HOST (defense-in-depth: even an auth
	// bug cannot be reached remotely by default). The Docker image sets
	// GO_HOST=0.0.0.0 so the published container port works.
	Host string

	// Port the Go backend listens on (the single public entry point).
	Port int

	// NodeUpstream is the base URL of the Node engine (open-sse) that receives
	// reverse-proxied requests, e.g. "http://127.0.0.1:20129".
	NodeUpstream string

	// DataDir is the directory used for SQLite storage. Kept here for Phase 2;
	// Phase 1 only passes it through.
	DataDir string

	// RequestBodyMaxBytes caps incoming request bodies (proxied + native).
	RequestBodyMaxBytes int64

	// ReadTimeout / WriteTimeout / ShutdownTimeout bound request lifecycles.
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration

	// LogLevel controls slog verbosity: "debug", "info", "warn", "error".
	LogLevel string
}

// Defaults match the documented .env.example contract.
const (
	defaultHost             = "127.0.0.1"
	defaultPort             = 20128
	defaultNodeUpstream     = "http://127.0.0.1:20129"
	defaultRequestBodyMaxMB = 128
)

// Load reads configuration from the process environment, applying sensible
// defaults for anything unset. It returns an error only if a present value is
// syntactically invalid (e.g. non-numeric PORT).
func Load() (Config, error) {
	cfg := Config{
		Host:                getenv("GO_HOST", getenv("HOST", defaultHost)),
		Port:                defaultPort,
		NodeUpstream:        getenv("NODE_UPSTREAM", defaultNodeUpstream),
		DataDir:             getenv("DATA_DIR", ""),
		RequestBodyMaxBytes: int64(getenvInt("GO_BODY_MAX_MB", defaultRequestBodyMaxMB)) * 1024 * 1024,
		ReadTimeout:         getenvDuration("GO_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:        getenvDuration("GO_WRITE_TIMEOUT", 5*time.Minute),
		ShutdownTimeout:     getenvDuration("GO_SHUTDOWN_TIMEOUT", 10*time.Second),
		LogLevel:            strings.ToLower(getenv("GO_LOG_LEVEL", "info")),
	}

	// PORT may be the shared one (20128). Allow override via GO_PORT for clarity.
	if v := os.Getenv("GO_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("GO_PORT: %w", err)
		}
		cfg.Port = p
	} else if v := os.Getenv("PORT"); v != "" {
		// PORT is shared with Node; if the Node upstream is already on PORT we must
		// not bind the same port. Only honour PORT for the Go listener when it
		// differs from the upstream port, otherwise keep the Go default.
		p, err := strconv.Atoi(v)
		if err != nil {
			return cfg, fmt.Errorf("PORT: %w", err)
		}
		if p != cfg.Port {
			cfg.Port = p
		}
	}

	cfg.NodeUpstream = strings.TrimRight(cfg.NodeUpstream, "/")
	if cfg.NodeUpstream == "" {
		return cfg, fmt.Errorf("NODE_UPSTREAM must not be empty")
	}

	// Resolve DataDir to the same location Node uses (dataDir.js): DATA_DIR env,
	// else ~/.9router. This must match so the auth guard reads the SAME
	// jwt-secret / machine-id / cli-secret files Node writes, and the DB opens
	// the same file. Leaving it "" would split them.
	if cfg.DataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg.DataDir = filepath.Join(home, ".9router")
		}
	}

	return cfg, nil
}

// Addr returns the listen address for the configured port.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
