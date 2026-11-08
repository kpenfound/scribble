package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Settings describes where and how the vault's daily notes live.
// Layout is a Go time layout produced by MomentLayout.
type Settings struct {
	Dir          string
	DailyDir     string
	Layout       string
	Location     *time.Location
	LookbackDays int
}

// Today formats t as the calendar date (YYYY-MM-DD) in the vault's time zone.
func (s Settings) Today(t time.Time) string { return t.In(s.Location).Format("2006-01-02") }

// MomentLayout explicitly rejects unsupported Moment tokens rather than
// silently file a note under a wrong name. Bracketed literals and nested
// year/month folders work.
func MomentLayout(format string) (string, error) {
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

// dailySettings lets .obsidian/daily-notes.json override the configured
// fallback folder and layout on every call.
func (s Settings) dailySettings(root *os.Root) (string, string, error) {
	b, err := root.ReadFile(".obsidian/daily-notes.json")
	if errors.Is(err, os.ErrNotExist) {
		return s.DailyDir, s.Layout, nil
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
	dir, layout := s.DailyDir, s.Layout
	if settings.Folder != nil {
		dir = *settings.Folder
	}
	if dir != "" && !filepath.IsLocal(dir) {
		return "", "", errors.New("configured Obsidian daily folder escapes vault")
	}
	if settings.Format != nil && *settings.Format != "" {
		layout, err = MomentLayout(*settings.Format)
	}
	return dir, layout, err
}
