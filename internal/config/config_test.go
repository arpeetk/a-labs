package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirPrecedence(t *testing.T) {
	t.Setenv("WREN_CONFIG_DIR", "/explicit")
	if Dir() != "/explicit" {
		t.Errorf("WREN_CONFIG_DIR should win, got %q", Dir())
	}
	t.Setenv("WREN_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if Dir() != "/xdg/wren" {
		t.Errorf("XDG_CONFIG_HOME should be used, got %q", Dir())
	}
	if Path() != filepath.Join(Dir(), "config.yaml") {
		t.Errorf("Path = %q", Path())
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	t.Setenv("WREN_CONFIG_DIR", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contexts) != 0 || cfg.CurrentContext != "" {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WREN_CONFIG_DIR", dir)
	cfg := &Config{CurrentContext: "acme"}
	cfg.Upsert(Context{Name: "acme", Server: "h:443", Org: "acme", Token: "secret"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// File must be owner-only (it can hold tokens).
	info, err := os.Stat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 600", perm)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "acme" || len(got.Contexts) != 1 || got.Contexts[0].Token != "secret" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLoadParseError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WREN_CONFIG_DIR", dir)
	// Unterminated flow sequence → YAML syntax error.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("contexts: [oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected parse error on malformed yaml")
	}
}

func TestUpsertAndResolve(t *testing.T) {
	c := &Config{}

	c.Upsert(Context{Name: "default", Server: "a:443"})
	c.Upsert(Context{Name: "default", Server: "b:443"}) // replace, not duplicate
	if len(c.Contexts) != 1 {
		t.Fatalf("expected 1 context after replace, got %d", len(c.Contexts))
	}
	if c.Contexts[0].Server != "b:443" {
		t.Fatalf("expected server b:443, got %q", c.Contexts[0].Server)
	}

	if _, err := c.Resolve(""); err == nil {
		t.Fatal("expected error resolving with no current context")
	}

	c.CurrentContext = "default"
	got, err := c.Resolve("")
	if err != nil {
		t.Fatalf("resolve current: %v", err)
	}
	if got.Server != "b:443" {
		t.Fatalf("expected b:443, got %q", got.Server)
	}

	if _, err := c.Resolve("nope"); err == nil {
		t.Fatal("expected error resolving unknown context")
	}
}
