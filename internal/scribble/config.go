package scribble

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Vault        string    `yaml:"vault"`
	DailyDir     string    `yaml:"daily_dir"`
	DateFormat   string    `yaml:"date_format"`
	Timezone     string    `yaml:"timezone"`
	LookbackDays int       `yaml:"lookback_days"`
	Listen       string    `yaml:"listen"`
	QueueDB      string    `yaml:"queue_db"`
	Jev          JevConfig `yaml:"jev"`
	Claude       struct {
		Model  string `yaml:"model"`
		Binary string `yaml:"binary"`
	} `yaml:"claude"`
	Transcribe struct {
		URL       string `yaml:"url"`
		Model     string `yaml:"model"`
		APIKeyEnv string `yaml:"api_key_env"`
	} `yaml:"transcribe"`
	MaxAttempts  int    `yaml:"max_attempts"`
	RetrySeconds int    `yaml:"retry_seconds"`
	LogLevel     string `yaml:"log_level"`
	location     *time.Location
	layout       string
}

type JevConfig struct {
	URL             string  `yaml:"url"`
	APIKeyEnv       string  `yaml:"api_key_env"`
	Model           string  `yaml:"model"`
	AmendConfidence float64 `yaml:"amend_confidence"`
	LinkConfidence  float64 `yaml:"link_confidence"`
	TagConfidence   float64 `yaml:"tag_confidence"`
}

func DefaultConfig() Config {
	c := Config{DailyDir: "Daily", DateFormat: "YYYY-MM-DD", Timezone: "America/Detroit", LookbackDays: 7, Listen: "127.0.0.1:8090", MaxAttempts: 3, RetrySeconds: 5, LogLevel: "info"}
	c.Jev = JevConfig{URL: "https://api.typesafe.ai", APIKeyEnv: "TYPESAFE_API_KEY", Model: "jev-latest", AmendConfidence: .75, LinkConfidence: .75, TagConfidence: .6}
	c.Claude.Model, c.Claude.Binary = "claude-haiku-5-5", "claude"
	c.Transcribe.Model, c.Transcribe.APIKeyEnv = "large-v3-turbo", "WHISPER_API_KEY"
	return c
}

func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.Vault == "" || c.QueueDB == "" {
		return errors.New("vault and queue_db are required")
	}
	var err error
	c.Vault, err = filepath.Abs(c.Vault)
	if err != nil {
		return err
	}
	c.QueueDB, err = filepath.Abs(c.QueueDB)
	if err != nil {
		return err
	}
	if c.location, err = time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.layout, err = momentLayout(c.DateFormat); err != nil {
		return err
	}
	if c.DailyDir != "" && !filepath.IsLocal(c.DailyDir) {
		return errors.New("daily_dir must be relative to the vault")
	}
	if c.LookbackDays < 1 || c.LookbackDays > 7 {
		return errors.New("lookback_days must be between 1 and 7")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 10 || c.RetrySeconds < 1 {
		return errors.New("invalid retry configuration")
	}
	for _, p := range []float64{c.Jev.AmendConfidence, c.Jev.LinkConfidence, c.Jev.TagConfidence} {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return errors.New("confidence thresholds must be between 0 and 1")
		}
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("listen must be a loopback IP and port; expose it with tailscale serve")
	}
	for _, raw := range []string{c.Jev.URL, c.Transcribe.URL} {
		if raw == "" {
			continue
		}
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("provider URLs must be absolute HTTP(S) URLs")
		}
	}
	if c.Jev.URL == "" || c.Jev.Model == "" || c.Jev.APIKeyEnv == "" || c.Claude.Model == "" || c.Claude.Binary == "" {
		return errors.New("model configuration is incomplete")
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return errors.New("invalid log_level")
	}
	return nil
}

// Explicitly reject unsupported Moment tokens rather than silently file a note
// under a wrong name. Bracketed literals and nested year/month folders work.
func momentLayout(format string) (string, error) {
	var out strings.Builder
	for len(format) > 0 {
		if format[0] == '[' {
			i := strings.IndexByte(format, ']')
			if i < 0 {
				return "", errors.New("unclosed date literal")
			}
			out.WriteString(format[1:i])
			format = format[i+1:]
			continue
		}
		matched := false
		for _, pair := range [][2]string{{"YYYY", "2006"}, {"MM", "01"}, {"DD", "02"}, {"M", "1"}, {"D", "2"}} {
			if strings.HasPrefix(format, pair[0]) {
				out.WriteString(pair[1])
				format = format[len(pair[0]):]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		ch := format[0]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' {
			return "", fmt.Errorf("unsupported date_format token near %q; use YYYY, MM, DD and [literals]", format)
		}
		out.WriteByte(ch)
		format = format[1:]
	}
	layout := out.String()
	if !filepath.IsLocal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC).Format(layout)+".md") || !strings.Contains(layout, "2006") || !strings.Contains(layout, "01") || !strings.Contains(layout, "02") {
		return "", errors.New("date_format must include YYYY, MM and DD and stay within the vault")
	}
	return layout, nil
}

func (c Config) dailySettings(root *os.Root) (string, string, error) {
	b, err := root.ReadFile(".obsidian/daily-notes.json")
	if errors.Is(err, os.ErrNotExist) {
		return c.DailyDir, c.layout, nil
	}
	if err != nil {
		return "", "", err
	}
	var settings struct {
		Folder *string `json:"folder"`
		Format *string `json:"format"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		return "", "", fmt.Errorf("daily-notes.json: %w", err)
	}
	dir, layout := c.DailyDir, c.layout
	if settings.Folder != nil {
		dir = *settings.Folder
	}
	if dir != "" && !filepath.IsLocal(dir) {
		return "", "", errors.New("configured Obsidian daily folder escapes vault")
	}
	if settings.Format != nil && *settings.Format != "" {
		layout, err = momentLayout(*settings.Format)
	}
	return dir, layout, err
}

func (c Config) today(now time.Time) string { return now.In(c.location).Format("2006-01-02") }
