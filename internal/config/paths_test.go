package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPathsHonourXDG(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("HOME", t.TempDir())
	p, err := ResolvePaths("")
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != filepath.Join(cfg, "scribble", "config.toml") {
		t.Fatalf("config %s", p.Config)
	}
	if p.Root != filepath.Join(data, "scribble") || p.Socket != filepath.Join(data, "scribble", "scribble.sock") {
		t.Fatalf("root %s socket %s", p.Root, p.Socket)
	}
}

func TestDefaultPathsFallBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "relative/dir"} { // unset and non-absolute both fall back
		t.Setenv("XDG_CONFIG_HOME", xdg)
		t.Setenv("XDG_DATA_HOME", xdg)
		p, err := ResolvePaths("")
		if err != nil {
			t.Fatal(err)
		}
		if p.Config != filepath.Join(home, ".config", "scribble", "config.toml") {
			t.Fatalf("config %s", p.Config)
		}
		if p.Root != filepath.Join(home, ".local", "share", "scribble") {
			t.Fatalf("root %s", p.Root)
		}
		if p.DB != filepath.Join(home, ".local/share/scribble/scribble.db") {
			t.Fatalf("db %s", p.DB)
		}
	}
}

func TestExplicitRootHoldsConfigAndAllState(t *testing.T) {
	// Even with XDG set, --root makes everything self-contained.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	p, err := ResolvePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"config.toml": p.Config, "scribble.db": p.DB, "scribble.db.lock": p.DBLock, "scribble.sock": p.Socket,
		"sessions": p.Sessions, "tailnet": p.Tailnet, "service.log": p.Log,
	} {
		if got != filepath.Join(root, name) {
			t.Errorf("%s resolved to %s", name, got)
		}
	}
	if p.Root != root || p.WriterDir != root {
		t.Fatalf("root %s writer dir %s", p.Root, p.WriterDir)
	}
}

// serve and every client command resolve through ResolvePaths, so one --root
// value names one socket and no other.
func TestSocketAgreesBetweenCallers(t *testing.T) {
	root := t.TempDir()
	a, err := ResolvePaths(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolvePaths(root)
	if err != nil || a != b {
		t.Fatalf("%+v %+v %v", a, b, err)
	}
	other, err := ResolvePaths(t.TempDir())
	if err != nil || other.Socket == a.Socket {
		t.Fatalf("different roots share a socket: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	def, err := ResolvePaths("")
	if err != nil || def.Socket == a.Socket {
		t.Fatalf("default root shares a socket with an explicit one: %v", err)
	}
}

func TestResolveRootErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePaths(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file as root: %v", err)
	}
	if _, err := ResolvePaths("/tmp/bad\nroot"); err == nil {
		t.Fatal("control characters accepted")
	}
	long := "/tmp/" + strings.Repeat("a", 100)
	if _, err := ResolvePaths(long); err == nil || !strings.Contains(err.Error(), "Unix socket limit") {
		t.Fatalf("overlong socket path: %v", err)
	}
}

func TestResolveCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := ResolvePaths(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root created: %v", err)
	}
}
