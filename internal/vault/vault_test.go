package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testSettings(t *testing.T) Settings {
	t.Helper()
	loc, err := time.LoadLocation("America/Detroit")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := MomentLayout("YYYY-MM-DD")
	if err != nil {
		t.Fatal(err)
	}
	return Settings{Dir: t.TempDir(), DailyDir: "Daily", Layout: layout, Location: loc, LookbackDays: 7}
}

func writeFixture(t *testing.T, root, path, text string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
}
func readFixture(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestScopedNoteEdits(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		t.Run(strings.ReplaceAll(nl, "\n", "LF"), func(t *testing.T) {
			original := strings.ReplaceAll("---\ntitle: Keep me\n---\n# Day\nprivate bytes  \n## Scribbles\n- old #idea ^scr-ab12\n\n## Journal\n- untouched ^scr-zzzz\n", "\n", nl)
			p := Plan{Block: "scr-ab12", Before: "- old #idea ^scr-ab12", After: "- revised #idea ^scr-ab12"}
			got, err := EditNote([]byte(original), p)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Replace(original, p.Before, p.After, 1)
			if string(got) != want {
				t.Fatalf("got %q want %q", got, want)
			}
			p.Before, p.After = p.After, p.Before
			restored, err := EditNote(got, p)
			if err != nil || string(restored) != original {
				t.Fatalf("undo: %q %v", restored, err)
			}
			p = Plan{Block: "scr-new1", After: "- new ^scr-new1"}
			added, err := EditNote([]byte(original), p)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(added, []byte("- new ^scr-new1"+nl+"## Journal")) {
				t.Fatalf("outside section: %s", added)
			}
			again, err := EditNote(added, p)
			if err != nil || !bytes.Equal(again, added) {
				t.Fatal("replay not idempotent")
			}
			p.Before, p.After = p.After, ""
			removed, err := EditNote(added, p)
			if err != nil || string(removed) != original {
				t.Fatalf("remove: %q %v", removed, err)
			}
		})
	}
}
func TestHeadingAndConflicts(t *testing.T) {
	original := []byte("# My existing note\n```md\n## Scribbles\n- example ^scr-test\n```\nlast byte")
	p := Plan{Block: "scr-new1", After: "- raw thought ^scr-new1"}
	got, err := EditNote(original, p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, original) || !strings.HasSuffix(string(got), "## Scribbles\n- raw thought ^scr-new1\n") {
		t.Fatalf("bad append: %q", got)
	}
	p.Before = p.After
	p.After = "- updated ^scr-new1"
	got = bytes.ReplaceAll(got, []byte("raw thought"), []byte("user edit"))
	if _, err = EditNote(got, p); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote user: %v", err)
	}
	if _, ok, err := findEntry(original, "scr-test"); ok || err != nil {
		t.Fatal("found fenced entry")
	}
}
func TestVaultRecoveryAndMidnight(t *testing.T) {
	c := testSettings(t)
	now := time.Date(2026, 11, 5, 23, 59, 0, 0, c.Location)
	v, err := Open(c, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	before := "# Existing\nDo not touch.\n"
	writeFixture(t, c.Dir, "Daily/2026-11-05.md", before)
	tag := Tag{Name: "#idea/talk", Meaning: "Talk ideas"}
	p := Plan{Date: "2026-11-05", Path: "Daily/2026-11-05.md", Block: "scr-test", After: "- hello #idea/talk ^scr-test", Tags: MakeTagEdits(nil, []string{tag.Name}, []Tag{tag}, "2026-11-05")}
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	note := readFixture(t, c.Dir, p.Path)
	registry := readFixture(t, c.Dir, TagsPath)
	// Restart/replay after both renames must neither duplicate the line nor count.
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	if readFixture(t, c.Dir, p.Path) != note || readFixture(t, c.Dir, TagsPath) != registry {
		t.Fatal("replayed write duplicated data")
	}
	if !strings.HasPrefix(note, before) {
		t.Fatal("changed original note")
	}
	now = now.Add(2 * time.Minute)
	if err = v.Apply(p); err != nil {
		t.Fatalf("already applied recovery across midnight: %v", err)
	}
	p.Block = "scr-next"
	p.After = "- tomorrow ^scr-next"
	if err = v.Apply(p); err == nil {
		t.Fatal("wrote to yesterday")
	}
}
func TestRegistryPreservesEditsAndProse(t *testing.T) {
	b := []byte("# Tags\n\n" + TagsHeader + "| #idea/talk | Talks | 2 | 2026-11-04 |\n\nKeep my notes.\n")
	tags, err := ParseTags(b)
	if err != nil {
		t.Fatal(err)
	}
	edits := MakeTagEdits(tags, []string{"#idea/talk", "#project/demo"}, []Tag{{Name: "#project/demo", Meaning: "Demo work"}}, "2026-11-05")
	got, err := EditTags(b, edits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte("\nKeep my notes.\n")) || !strings.Contains(string(got), "| Talks | 3 | 2026-11-05 |") {
		t.Fatalf("bad registry: %s", got)
	}
	replay, err := EditTags(got, edits)
	if err != nil || !bytes.Equal(got, replay) {
		t.Fatal("registry replay not idempotent")
	}
	b = bytes.ReplaceAll(b, []byte("Talks"), []byte("My revised meaning"))
	if _, err := EditTags(b, edits); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote user meaning: %v", err)
	}
}
func TestContextCalendarDaysAndSettings(t *testing.T) {
	c := testSettings(t)
	now := time.Date(2026, 11, 5, 1, 30, 0, 0, time.UTC) // Still Nov 4 in Detroit.
	writeFixture(t, c.Dir, ".obsidian/daily-notes.json", `{"folder":"Journal","format":"YYYY/MM/YYYY-MM-DD"}`)
	for _, date := range []string{"2026-11-04", "2026-10-28", "2026-10-27"} {
		parsed, _ := time.Parse("2006-01-02", date)
		writeFixture(t, c.Dir, "Journal/"+parsed.Format("2006/01/2006-01-02")+".md", "## Scribbles\n- "+date+" ^scr-same\n")
	}
	v, err := Open(c, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	entries, _, err := v.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Date != "2026-11-04" || entries[0].ID == entries[1].ID {
		t.Fatalf("calendar context: %+v", entries)
	}
	path, err := v.DailyPath(now)
	if err != nil || path != "Journal/2026/11/2026-11-04.md" {
		t.Fatalf("path %s %v", path, err)
	}
}
func TestVaultRejectsEscapingSymlink(t *testing.T) {
	c := testSettings(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(c.Dir, "Daily")); err != nil {
		t.Fatal(err)
	}
	v, err := Open(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	p := Plan{Date: c.Today(time.Now()), Path: "Daily/test.md", Block: "scr-test", After: "- test ^scr-test"}
	if err := v.Apply(p); err == nil {
		t.Fatal("escaped vault")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) > 0 {
		t.Fatal("wrote outside vault")
	}
}
func TestVaultHasOneWriterAcrossDifferentQueues(t *testing.T) {
	c := testSettings(t)
	first, err := Open(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Open(c, nil); err == nil {
		_ = second.Close()
		t.Fatal("another process can write the same vault")
	}
}
func TestConcurrentUnrelatedEditIsRebased(t *testing.T) {
	c := testSettings(t)
	v, err := Open(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	path, _ := v.DailyPath(time.Now())
	original := "# Notes\n## Scribbles\n- initial ^scr-test\n"
	writeFixture(t, c.Dir, path, original)
	p := Plan{Block: "scr-test", Before: "- initial ^scr-test", After: "- amended ^scr-test"}
	attempts := 0
	err = v.update(path, func() error { return nil }, func(b []byte) ([]byte, error) {
		attempts++
		if attempts == 1 {
			writeFixture(t, c.Dir, path, "User added this\n"+original)
		}
		return EditNote(b, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected a rebase, got %d attempts", attempts)
	}
	if readFixture(t, c.Dir, path) != "User added this\n# Notes\n## Scribbles\n- amended ^scr-test\n" {
		t.Fatal("lost concurrent user edit")
	}
}

func TestMomentLayoutRejectsUnsupportedFormats(t *testing.T) {
	for _, format := range []string{"../YYYY-MM-DD", "YYYY-ww-DD", "YYYY-MM", "[bad"} {
		if _, err := MomentLayout(format); err == nil {
			t.Fatalf("accepted %s", format)
		}
	}
}
func TestDailySettingsRejectEscapingFolder(t *testing.T) {
	c := testSettings(t)
	writeFixture(t, c.Dir, ".obsidian/daily-notes.json", `{"folder":"../escape"}`)
	v, err := Open(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err = v.DailyPath(time.Now()); err == nil {
		t.Fatal("escaping config accepted")
	}
}
