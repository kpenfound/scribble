package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Paths locates the configuration file and every piece of state scribble keeps.
// serve and the client commands resolve them through ResolvePaths, so they
// always agree on the socket.
type Paths struct {
	// Root is the state root.
	Root string `json:"root"`
	// Config is the config.toml that is read. With an explicit root it is
	// <Root>/config.toml; by default it lives in the XDG config directory.
	Config string `json:"config"`
	// DB is the SQLite queue database; its write-ahead files sit beside it.
	DB string `json:"db"`
	// DBLock is the database's flock file.
	DBLock string `json:"db_lock"`
	// Socket is the Unix socket the service listens on.
	Socket string `json:"socket"`
	// Sessions holds the agent sessions the writer creates.
	Sessions string `json:"sessions"`
	// WriterDir is the parent of the writer-* temporary workspaces.
	WriterDir string `json:"writer_dir"`
	// Tailnet is the embedded Tailscale node's state directory.
	Tailnet string `json:"tailnet"`
	// Log is where a detached service writes its log.
	Log string `json:"log"`
}

// maxSocketPath is the longest socket path that fits Darwin's sockaddr_un,
// with a trailing NUL. Linux allows slightly more.
const maxSocketPath = 103

// PathsAt lays out the state under root. The config file is root/config.toml.
// root is not made absolute or created.
func PathsAt(root string) Paths {
	return Paths{
		Root:      root,
		Config:    filepath.Join(root, "config.toml"),
		DB:        filepath.Join(root, "scribble.db"),
		DBLock:    filepath.Join(root, "scribble.db.lock"),
		Socket:    filepath.Join(root, "scribble.sock"),
		Sessions:  filepath.Join(root, "sessions"),
		WriterDir: root,
		Tailnet:   filepath.Join(root, "tailnet"),
		Log:       filepath.Join(root, "service.log"),
	}
}

// ResolvePaths is the one root-resolution API. A non-empty explicit root (from
// --root) is a self-contained root that holds config.toml and all state.
// Otherwise the config is $XDG_CONFIG_HOME/scribble/config.toml and the root is
// $XDG_DATA_HOME/scribble; each variable is used only when absolute, and falls
// back to ~/.config and ~/.local/share. Nothing is created.
func ResolvePaths(explicit string) (Paths, error) {
	return resolvePaths(explicit, "")
}

// resolvePaths takes the home directory as a parameter for tests; empty means
// os.UserHomeDir, consulted only when a fallback needs it.
func resolvePaths(explicit, home string) (Paths, error) {
	configFile := ""
	if explicit == "" {
		var err error
		if explicit, err = xdgDirectory("XDG_DATA_HOME", ".local/share", home, "scribble"); err != nil {
			return Paths{}, fmt.Errorf("root: %w", err)
		}
		if configFile, err = xdgDirectory("XDG_CONFIG_HOME", ".config", home, "scribble", "config.toml"); err != nil {
			return Paths{}, fmt.Errorf("config: %w", err)
		}
	}
	if strings.ContainsAny(explicit, "\x00\r\n") {
		return Paths{}, errors.New("root: path contains control characters")
	}
	root, err := filepath.Abs(explicit)
	if err != nil {
		return Paths{}, fmt.Errorf("root: %w", err)
	}
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		return Paths{}, fmt.Errorf("root: %s is not a directory", root)
	}
	p := PathsAt(root)
	if configFile != "" {
		p.Config = configFile
	}
	if len(p.Socket) > maxSocketPath {
		return Paths{}, fmt.Errorf("root: socket path %s exceeds the %d-byte Unix socket limit; choose a shorter root", p.Socket, maxSocketPath)
	}
	return p, nil
}

func xdgDirectory(variable, fallback, home string, parts ...string) (string, error) {
	base := os.Getenv(variable)
	if !filepath.IsAbs(base) {
		if home == "" {
			var err error
			if home, err = os.UserHomeDir(); err != nil {
				return "", err
			}
		}
		if !filepath.IsAbs(home) {
			return "", errors.New("home directory must be absolute")
		}
		base = filepath.Join(home, fallback)
	}
	return filepath.Join(append([]string{base}, parts...)...), nil
}
