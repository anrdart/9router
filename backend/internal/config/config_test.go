package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("NODE_UPSTREAM", "")
	t.Setenv("PORT", "")
	t.Setenv("GO_PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.NodeUpstream != defaultNodeUpstream {
		t.Errorf("NodeUpstream = %q, want %q", cfg.NodeUpstream, defaultNodeUpstream)
	}
	if cfg.RequestBodyMaxBytes != defaultRequestBodyMaxMB*1024*1024 {
		t.Errorf("RequestBodyMaxBytes = %d, want %d", cfg.RequestBodyMaxBytes, defaultRequestBodyMaxMB*1024*1024)
	}
	if cfg.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", cfg.ReadTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("GO_PORT", "9999")
	t.Setenv("NODE_UPSTREAM", "http://upstream.local:3000/")
	t.Setenv("GO_BODY_MAX_MB", "10")
	t.Setenv("GO_READ_TIMEOUT", "5s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	// Trailing slash must be trimmed.
	if cfg.NodeUpstream != "http://upstream.local:3000" {
		t.Errorf("NodeUpstream = %q, want trimmed", cfg.NodeUpstream)
	}
	if cfg.RequestBodyMaxBytes != 10*1024*1024 {
		t.Errorf("RequestBodyMaxBytes = %d, want 10MB", cfg.RequestBodyMaxBytes)
	}
	if cfg.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %v, want 5s", cfg.ReadTimeout)
	}
}

func TestLoadInvalidPort(t *testing.T) {
	t.Setenv("GO_PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-numeric GO_PORT")
	}
}

func TestLoadHonoursSharedPortWhenDistinct(t *testing.T) {
	t.Setenv("PORT", "5555")
	t.Setenv("GO_PORT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 5555 {
		t.Errorf("Port = %d, want 5555 (from shared PORT)", cfg.Port)
	}
}

func TestLoadKeepsGoDefaultWhenSharedPortEqualsUpstream(t *testing.T) {
	// If PORT equals the default Go port (avoiding binding same port as upstream),
	// keep default.
	t.Setenv("PORT", "20128")
	t.Setenv("GO_PORT", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want default %d", cfg.Port, defaultPort)
	}
}

func TestAddr(t *testing.T) {
	if got := (Config{Host: "127.0.0.1", Port: 20128}).Addr(); got != "127.0.0.1:20128" {
		t.Errorf("Addr = %q, want 127.0.0.1:20128", got)
	}
	if got := (Config{Host: "0.0.0.0", Port: 20128}).Addr(); got != "0.0.0.0:20128" {
		t.Errorf("Addr = %q, want 0.0.0.0:20128", got)
	}
}

// TestLoadDefaultHost verifies the bind host defaults to loopback and honors
// GO_HOST (defense-in-depth: not network-reachable unless explicitly exposed).
func TestLoadDefaultHost(t *testing.T) {
	t.Setenv("GO_HOST", "")
	t.Setenv("HOST", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("default Host = %q, want 127.0.0.1", cfg.Host)
	}

	t.Setenv("GO_HOST", "0.0.0.0")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.Host != "0.0.0.0" {
		t.Errorf("GO_HOST override = %q, want 0.0.0.0", cfg2.Host)
	}
}
