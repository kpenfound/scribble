// Package config loads scribble's TOML configuration and resolves the paths of
// its state root. Loading never creates files or starts anything.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the configured time zone must resolve on minimal hosts

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/scribble/internal/vault"
)

// Version is the only config format version.
const Version = 1

// AgentClaude is the only agent a profile may name.
const AgentClaude = "claude"

// Config is the loaded configuration. The struct tags are the file's keys and
// the JSON shape that `scribble config` prints.
type Config struct {
	Version      int    `toml:"version" json:"version"`
	Vault        string `toml:"vault" json:"vault"`
	Timezone     string `toml:"timezone" json:"timezone"`
	LookbackDays int    `toml:"lookback_days" json:"lookback_days"`
	LogLevel     string `toml:"log_level" json:"log_level"`

	Daily      Daily      `toml:"daily" json:"daily"`
	Listen     Listen     `toml:"listen" json:"listen"`
	Queue      Queue      `toml:"queue" json:"queue"`
	Jev        Jev        `toml:"jev" json:"jev"`
	Profiles   Profiles   `toml:"profiles" json:"profiles"`
	Transcribe Transcribe `toml:"transcribe" json:"transcribe"`

	// Paths is where the config was read from and where state lives. It is not
	// a file key. DefaultConfig leaves it empty; Load and tests fill it in.
	Paths Paths `toml:"-" json:"paths"`

	location *time.Location
	layout   string
}

// Daily is used only when .obsidian/daily-notes.json is absent.
type Daily struct {
	Folder string `toml:"folder" json:"folder"`
	Format string `toml:"format" json:"format"`
}

// Listen configures the listeners besides the always-on Unix socket.
type Listen struct {
	Web          string `toml:"web" json:"web"`
	Tailnet      string `toml:"tailnet" json:"tailnet"`
	TailnetHTTPS bool   `toml:"tailnet_https" json:"tailnet_https"`
}

// Queue bounds automatic attempts at a job.
type Queue struct {
	MaxAttempts  int    `toml:"max_attempts" json:"max_attempts"`
	RetryBackoff string `toml:"retry_backoff" json:"retry_backoff"`
}

// Jev configures routing. The thresholds are probabilities in [0,1].
type Jev struct {
	URL            string  `toml:"url" json:"url"`
	Model          string  `toml:"model" json:"model"`
	APIKeyEnv      string  `toml:"api_key_env" json:"api_key_env"`
	Timeout        string  `toml:"timeout" json:"timeout"`
	AmendThreshold float64 `toml:"amend_threshold" json:"amend_threshold"`
	LinkThreshold  float64 `toml:"link_threshold" json:"link_threshold"`
	TagThreshold   float64 `toml:"tag_threshold" json:"tag_threshold"`
}

// Profiles holds the agent profiles. The writer role uses Default.
type Profiles struct {
	Default Profile `toml:"default" json:"default"`
}

// Profile names an agent and how to run it.
type Profile struct {
	Agent   string `toml:"agent" json:"agent"`
	Model   string `toml:"model" json:"model"`
	Timeout string `toml:"timeout" json:"timeout"`
}

// Transcribe configures voice capture. An empty URL means voice jobs fail
// retryably. URL is the full transcription endpoint.
type Transcribe struct {
	URL       string `toml:"url" json:"url"`
	Model     string `toml:"model" json:"model"`
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env"`
}

// ExampleTOML is internal/config/config.example.toml, the commented example
// config. It is embedded so tests (and anything that wants to print it) read
// the same bytes the repository ships.
//
//go:embed config.example.toml
var ExampleTOML []byte

// DefaultConfig is every default. config.example.toml states the same values;
// it adds only a placeholder vault, which has no default and is required.
func DefaultConfig() Config {
	return Config{
		Version:      Version,
		Timezone:     "America/Detroit",
		LookbackDays: 7,
		LogLevel:     "info",
		Daily:        Daily{Folder: "Daily", Format: "YYYY-MM-DD"},
		Listen:       Listen{Tailnet: "scribble", TailnetHTTPS: true},
		Queue:        Queue{MaxAttempts: 3, RetryBackoff: "5s"},
		Jev: Jev{
			URL:            "https://api.typesafe.ai",
			Model:          "jev-latest",
			APIKeyEnv:      "TYPESAFE_API_KEY",
			Timeout:        "10s",
			AmendThreshold: .75,
			LinkThreshold:  .75,
			TagThreshold:   .6,
		},
		Profiles:   Profiles{Default: Profile{Agent: AgentClaude, Model: "claude-haiku-5-5", Timeout: "90s"}},
		Transcribe: Transcribe{Model: "large-v3-turbo", APIKeyEnv: "WHISPER_API_KEY"},
	}
}

// FieldError is a configuration file that failed to load: the file, the field
// at fault (empty when the whole file is at fault) and why. Its text never
// quotes values from the file.
type FieldError struct {
	Path, Field, Reason string
	Err                 error
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Path + ": " + e.Reason
	}
	return e.Path + ": " + e.Field + ": " + e.Reason
}

func (e *FieldError) Unwrap() error { return e.Err }

func fieldError(path, field, reason string) error {
	return &FieldError{Path: path, Field: field, Reason: reason}
}

// Load reads and validates the config file that paths names. A successful
// Load has Paths set.
func Load(paths Paths) (Config, error) {
	data, err := os.ReadFile(paths.Config)
	if err != nil {
		return Config{}, &FieldError{Path: paths.Config, Reason: "cannot be read", Err: err}
	}
	c, err := Parse(paths.Config, data)
	if err != nil {
		return Config{}, err
	}
	c.Paths = paths
	return c, nil
}

// durationFields are the keys that must be duration strings.
var durationFields = []string{"queue.retry_backoff", "jev.timeout", "profiles.default.timeout"}

// Parse decodes and validates config text. path only labels errors. Keys that
// are absent keep their defaults, except version, which must be present.
func Parse(path string, data []byte) (Config, error) {
	c := DefaultConfig()
	c.Version = 0
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return Config{}, decodeError(path, err)
	}
	if key, ok := unknownKey(md.Undecoded()); ok {
		return Config{}, fieldError(path, key, "unknown key")
	}
	if c.Version != Version {
		return Config{}, fieldError(path, "version", "must be 1")
	}
	if err = c.validate(path); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate checks a config built in code. Errors are labelled "config".
func (c *Config) Validate() error { return c.validate("config") }

// unknownKey picks the most specific undecoded key: the first one that no
// other undecoded key extends, so [claude] binary = "x" reports claude.binary.
func unknownKey(keys []toml.Key) (string, bool) {
	for _, k := range keys {
		leaf := true
		for _, other := range keys {
			if len(other) > len(k) && slices.Equal(other[:len(k)], k) {
				leaf = false
				break
			}
		}
		if leaf {
			return k.String(), true
		}
	}
	return "", false
}

// typeMismatch matches the decoder's message for a value of the wrong type,
// which carries the key and line.
var typeMismatch = regexp.MustCompile(`^toml: (?:line ([0-9]+) )?\(last key ("(?:[^"\\]|\\.)*")\): (?:incompatible types|type mismatch)`)

// decodeError names the key and line of a file the decoder rejected, leaving
// out the decoder's text, which may quote the file.
func decodeError(path string, err error) error {
	var parse toml.ParseError
	if errors.As(err, &parse) {
		return &FieldError{Path: path, Field: parse.LastKey, Reason: fmt.Sprintf("invalid TOML at line %d", parse.Position.Line), Err: err}
	}
	if m := typeMismatch.FindStringSubmatch(err.Error()); m != nil {
		key, _ := strconv.Unquote(m[2])
		reason := "value has the wrong type"
		if slices.Contains(durationFields, key) {
			reason = `must be a duration string such as "5s"`
		}
		if m[1] != "" {
			reason += " (line " + m[1] + ")"
		}
		return &FieldError{Path: path, Field: key, Reason: reason, Err: err}
	}
	return &FieldError{Path: path, Reason: "invalid TOML", Err: err}
}

func (c *Config) validate(path string) error {
	if c.Version != Version {
		return fieldError(path, "version", "must be 1")
	}
	if c.Vault == "" {
		return fieldError(path, "vault", "is required")
	}
	abs, err := filepath.Abs(c.Vault)
	if err != nil {
		return fieldError(path, "vault", err.Error())
	}
	c.Vault = abs
	if c.location, err = time.LoadLocation(c.Timezone); err != nil {
		return fieldError(path, "timezone", "unknown time zone")
	}
	if c.LookbackDays < 1 || c.LookbackDays > 7 {
		return fieldError(path, "lookback_days", "must be between 1 and 7")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		return fieldError(path, "log_level", "must be debug, info, warn or error")
	}
	if c.Daily.Folder != "" && !filepath.IsLocal(c.Daily.Folder) {
		return fieldError(path, "daily.folder", "must be relative to the vault")
	}
	if c.layout, err = vault.MomentLayout(c.Daily.Format); err != nil {
		return fieldError(path, "daily.format", err.Error())
	}
	if c.Listen.Web != "" {
		if err = validateWeb(c.Listen.Web); err != nil {
			return fieldError(path, "listen.web", err.Error())
		}
	}
	if c.Listen.Tailnet != "" {
		if err = validateTailnet(c.Listen.Tailnet); err != nil {
			return fieldError(path, "listen.tailnet", err.Error())
		}
	}
	if c.Queue.MaxAttempts < 1 || c.Queue.MaxAttempts > 10 {
		return fieldError(path, "queue.max_attempts", "must be between 1 and 10")
	}
	if err = validateDuration(c.Queue.RetryBackoff); err != nil {
		return fieldError(path, "queue.retry_backoff", err.Error())
	}
	if err = validateURL(c.Jev.URL, true); err != nil {
		return fieldError(path, "jev.url", err.Error())
	}
	for _, f := range []struct{ field, value string }{{"jev.model", c.Jev.Model}, {"jev.api_key_env", c.Jev.APIKeyEnv}} {
		if f.value == "" {
			return fieldError(path, f.field, "is required")
		}
	}
	if err = validateDuration(c.Jev.Timeout); err != nil {
		return fieldError(path, "jev.timeout", err.Error())
	}
	for _, f := range []struct {
		field string
		value float64
	}{{"jev.amend_threshold", c.Jev.AmendThreshold}, {"jev.link_threshold", c.Jev.LinkThreshold}, {"jev.tag_threshold", c.Jev.TagThreshold}} {
		if math.IsNaN(f.value) || f.value < 0 || f.value > 1 {
			return fieldError(path, f.field, "must be between 0 and 1")
		}
	}
	p := c.Profiles.Default
	if p.Agent != AgentClaude {
		return fieldError(path, "profiles.default.agent", `must be "claude"; no other agent is supported`)
	}
	if p.Model == "" {
		return fieldError(path, "profiles.default.model", "is required")
	}
	if err = validateDuration(p.Timeout); err != nil {
		return fieldError(path, "profiles.default.timeout", err.Error())
	}
	if err = validateURL(c.Transcribe.URL, false); err != nil {
		return fieldError(path, "transcribe.url", err.Error())
	}
	if c.Transcribe.Model == "" {
		return fieldError(path, "transcribe.model", "is required")
	}
	return nil
}

func validateDuration(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return errors.New(`must be a duration string such as "5s"`)
	}
	if d <= 0 {
		return errors.New("must be positive")
	}
	return nil
}

func validateURL(raw string, required bool) error {
	if raw == "" {
		if required {
			return errors.New("is required")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("must be an absolute http or https URL")
	}
	return nil
}

// validateWeb accepts host:port where the host is localhost or a loopback IP.
func validateWeb(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("expected host:port, such as 127.0.0.1:8090")
	}
	if host == "" {
		return errors.New("host is required; use localhost, 127.0.0.1 or [::1]")
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || strconv.FormatUint(n, 10) != port {
		return errors.New("port must be a number from 0 to 65535")
	}
	if !LoopbackHost(host) {
		return errors.New("host must be localhost or a loopback address; reach it from other devices over the tailnet")
	}
	return nil
}

// LoopbackHost reports whether host, without port or brackets, is localhost or
// a loopback IP address.
func LoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateTailnet accepts a single DNS label: 1 to 63 lowercase letters, digits
// and hyphens that neither starts nor ends with a hyphen.
func validateTailnet(hostname string) error {
	if len(hostname) > 63 {
		return errors.New("hostname must be at most 63 characters")
	}
	for _, r := range hostname {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return errors.New("hostname must be one label of lowercase letters, digits and hyphens, such as scribble")
		}
	}
	if strings.HasPrefix(hostname, "-") || strings.HasSuffix(hostname, "-") {
		return errors.New("hostname must not start or end with a hyphen")
	}
	return nil
}

// duration parses a validated duration string; invalid text yields zero.
func duration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}

// RetryBackoff is queue.retry_backoff.
func (c Config) RetryBackoff() time.Duration { return duration(c.Queue.RetryBackoff) }

// JevTimeout is jev.timeout.
func (c Config) JevTimeout() time.Duration { return duration(c.Jev.Timeout) }

// WriterTimeout is the default profile's timeout.
func (c Config) WriterTimeout() time.Duration { return duration(c.Profiles.Default.Timeout) }

// Location is the configured time zone; valid after validation.
func (c Config) Location() *time.Location { return c.location }

// Today formats now as the calendar date in the configured time zone.
func (c Config) Today(now time.Time) string { return now.In(c.location).Format("2006-01-02") }

// VaultSettings is the view of the config that the vault package needs.
func (c Config) VaultSettings() vault.Settings {
	return vault.Settings{Dir: c.Vault, DailyDir: c.Daily.Folder, Layout: c.layout, Location: c.location, LookbackDays: c.LookbackDays}
}
