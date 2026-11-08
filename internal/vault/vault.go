package vault

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const TagsPath = "Scribble/Tags.md"
const TagsHeader = "| Tag | Meaning | Uses | Last used |\n| --- | --- | --- | --- |\n"

var BlockRE = regexp.MustCompile(` \^(scr-[a-z0-9]{4})$`)
var TagRE = regexp.MustCompile(`^#[a-zA-Z_][a-zA-Z0-9_-]*(/[a-zA-Z0-9_-]+)*$`)
var inlineTagRE = regexp.MustCompile(`(?:^|\s)(#[a-zA-Z_][a-zA-Z0-9_-]*(?:/[a-zA-Z0-9_-]+)*)`)
var ErrConflict = errors.New("vault content changed; refusing to overwrite an edited entry")

type Entry struct {
	ID    string `json:"id"`
	Block string `json:"block"`
	Path  string `json:"path"`
	Date  string `json:"date"`
	Line  string `json:"line"`
}
type Tag struct {
	Name     string `json:"name"`
	Meaning  string `json:"meaning"`
	Uses     int    `json:"uses"`
	LastUsed string `json:"last_used"`
	Raw      string `json:"-"`
}
type RowEdit struct {
	Name   string
	Before string
	After  string
}
type Plan struct {
	Date   string    `json:"date"`
	Path   string    `json:"path"`
	Block  string    `json:"block"`
	Before string    `json:"before"`
	After  string    `json:"after"`
	Action string    `json:"action"`
	Tags   []RowEdit `json:"tag_edits"`
	Undo   bool      `json:"undo"`
}
type Vault struct {
	lock     *os.File
	root     *os.Root
	settings Settings
	now      func() time.Time
}

// Open locks and opens the vault. now is injectable so tests can cross midnight.
func Open(s Settings, now func() time.Time) (*Vault, error) {
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return nil, err
	}
	lock, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		_ = root.Close()
		return nil, errors.New("another scribble process owns this vault")
	}
	if now == nil {
		now = time.Now
	}
	return &Vault{root: root, lock: lock, settings: s, now: now}, nil
}
func (v *Vault) Close() error {
	_ = unix.Flock(int(v.lock.Fd()), unix.LOCK_UN)
	_ = v.lock.Close()
	return v.root.Close()
}

// Read returns the file bytes, or nil when the file does not exist.
func (v *Vault) Read(path string) ([]byte, error) {
	b, e := v.root.ReadFile(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	return b, e
}

// DailyPath returns the vault-relative daily note path for t.
func (v *Vault) DailyPath(t time.Time) (string, error) {
	dir, layout, err := v.settings.dailySettings(v.root)
	return filepath.Join(dir, t.In(v.settings.Location).Format(layout)+".md"), err
}

// Context returns the lookback-window entries and the tag registry.
func (v *Vault) Context() ([]Entry, []Tag, error) {
	now := v.now().In(v.settings.Location)
	var entries []Entry
	for i := 0; i <= v.settings.LookbackDays; i++ {
		date := now.AddDate(0, 0, -i)
		path, err := v.DailyPath(date)
		if err != nil {
			return nil, nil, err
		}
		b, err := v.Read(path)
		if err != nil {
			return nil, nil, err
		}
		for _, line := range scribbleLines(b) {
			m := BlockRE.FindStringSubmatch(line)
			if m != nil && strings.HasPrefix(line, "- ") {
				entries = append(entries, Entry{ID: filepath.ToSlash(path) + "#^" + m[1], Block: m[1], Path: path, Date: v.settings.Today(date), Line: line})
			}
		}
	}
	b, err := v.Read(TagsPath)
	if err != nil {
		return nil, nil, err
	}
	tags, err := ParseTags(b)
	return entries, tags, err
}

type lineSpan struct {
	start, end int
	text       string
}

func lines(b []byte) []lineSpan {
	var spans []lineSpan
	for start := 0; start < len(b); {
		n := bytes.IndexByte(b[start:], '\n')
		end := len(b)
		if n >= 0 {
			end = start + n + 1
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(b[start:end]), "\n"), "\r")
		spans = append(spans, lineSpan{start, end, text})
		start = end
	}
	return spans
}

// Only an actual level-two heading outside code fences starts the section.
func section(b []byte) (int, int, bool) {
	start, end := -1, len(b)
	fence := ""
	for _, l := range lines(b) {
		t := strings.TrimSpace(l.text)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			marker := t[:3]
			if fence == "" {
				fence = marker
			} else if marker == fence {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if start >= 0 && (strings.HasPrefix(l.text, "## ") || strings.HasPrefix(l.text, "# ")) {
			end = l.start
			break
		}
		if strings.TrimRight(l.text, " \t") == "## Scribbles" {
			if start >= 0 {
				return 0, 0, false
			}
			start = l.end
		}
	}
	return start, end, start >= 0
}
func scribbleLines(b []byte) []string {
	start, end, ok := section(b)
	if !ok {
		return nil
	}
	var out []string
	fence := ""
	for _, l := range lines(b[start:end]) {
		t := strings.TrimSpace(l.text)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if fence == "" {
				fence = t[:3]
			} else if fence == t[:3] {
				fence = ""
			}
			continue
		}
		if fence == "" {
			out = append(out, l.text)
		}
	}
	return out
}

// FindEntry reports the text of the scribble line carrying block, if present.
func FindEntry(b []byte, block string) (string, bool, error) {
	l, found, err := findEntry(b, block)
	return l.text, found, err
}
func findEntry(b []byte, block string) (lineSpan, bool, error) {
	start, end, ok := section(b)
	if !ok {
		return lineSpan{}, false, nil
	}
	var match lineSpan
	found := false
	valid := map[string]bool{}
	for _, s := range scribbleLines(b) {
		valid[s] = true
	}
	for _, l := range lines(b[start:end]) {
		if valid[l.text] && strings.HasPrefix(l.text, "- ") && strings.HasSuffix(l.text, " ^"+block) {
			if found {
				return match, false, errors.New("duplicate scribble block ID")
			}
			found = true
			match = l
			match.start += start
			match.end += start
		}
	}
	return match, found, nil
}
func newline(b []byte) string {
	if bytes.Contains(b, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}
func EditNote(b []byte, p Plan) ([]byte, error) {
	l, found, err := findEntry(b, p.Block)
	if err != nil {
		return nil, err
	}
	if found && l.text == p.After {
		return b, nil
	}
	if p.After == "" && !found {
		return b, nil
	}
	if p.Before != "" {
		if !found || l.text != p.Before {
			return nil, ErrConflict
		}
		replacement := []byte{}
		if p.After != "" {
			suffix := b[l.start+len(l.text) : l.end]
			replacement = append([]byte(p.After), suffix...)
		}
		out := append(append(append([]byte{}, b[:l.start]...), replacement...), b[l.end:]...)
		// Verify that replacing/removing exactly one physical line left all other bytes alone.
		if !bytes.Equal(append(append([]byte{}, b[:l.start]...), b[l.end:]...), append(append([]byte{}, out[:l.start]...), out[l.start+len(replacement):]...)) {
			return nil, errors.New("scoped edit invariant failed")
		}
		return out, nil
	}
	if found {
		return nil, ErrConflict
	}
	nl := newline(b)
	_, end, ok := section(b)
	out := append([]byte{}, b...)
	if !ok {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, []byte(nl)...)
		}
		out = append(out, []byte(nl+"## Scribbles"+nl)...)
		end = len(out)
	}
	insert := p.After + nl
	if end > 0 && out[end-1] != '\n' {
		insert = nl + insert
	}
	return append(append(append([]byte{}, out[:end]...), []byte(insert)...), out[end:]...), nil
}
func NewBlock(b []byte) (string, error) {
	for i := 0; i < 100; i++ {
		var s strings.Builder
		s.WriteString("scr-")
		for j := 0; j < 4; j++ {
			n, err := rand.Int(rand.Reader, big.NewInt(36))
			if err != nil {
				return "", err
			}
			s.WriteByte("0123456789abcdefghijklmnopqrstuvwxyz"[n.Int64()])
		}
		id := s.String()
		if !bytes.Contains(b, []byte("^"+id)) {
			return id, nil
		}
	}
	return "", errors.New("unable to allocate unique block ID")
}
func ParseTags(b []byte) ([]Tag, error) {
	var tags []Tag
	seen := map[string]bool{}
	for _, l := range lines(b) {
		cells := strings.Split(strings.TrimSpace(l.text), "|")
		if len(cells) < 3 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "#") {
			continue
		}
		if len(cells) != 6 {
			return nil, errors.New("invalid tag table row")
		}
		name := strings.TrimSpace(cells[1])
		uses, err := strconv.Atoi(strings.TrimSpace(cells[3]))
		if !TagRE.MatchString(name) || err != nil || uses < 0 || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate registry tag %q", name)
		}
		seen[name] = true
		tags = append(tags, Tag{Name: name, Meaning: strings.TrimSpace(cells[2]), Uses: uses, LastUsed: strings.TrimSpace(cells[4]), Raw: l.text})
	}
	return tags, nil
}
func tagLine(t Tag) string {
	return fmt.Sprintf("| %s | %s | %d | %s |", t.Name, t.Meaning, t.Uses, t.LastUsed)
}
func TagsIn(line string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range inlineTagRE.FindAllStringSubmatch(line, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}
func MakeTagEdits(registry []Tag, names []string, created []Tag, date string) []RowEdit {
	byName := map[string]Tag{}
	for _, t := range registry {
		byName[t.Name] = t
	}
	fresh := map[string]Tag{}
	for _, t := range created {
		fresh[t.Name] = t
	}
	var edits []RowEdit
	for _, name := range names {
		t, ok := byName[name]
		before := t.Raw
		if !ok {
			t, ok = fresh[name]
			if !ok {
				continue
			}
		}
		t.Uses++
		t.LastUsed = date
		edits = append(edits, RowEdit{Name: name, Before: before, After: tagLine(t)})
	}
	return edits
}
func EditTags(b []byte, edits []RowEdit) ([]byte, error) {
	if _, err := ParseTags(b); err != nil {
		return nil, err
	}
	out := append([]byte{}, b...)
	if len(out) == 0 {
		out = []byte(TagsHeader)
	}
	for _, edit := range edits {
		var current *lineSpan
		for _, l := range lines(out) {
			cells := strings.Split(strings.TrimSpace(l.text), "|")
			if len(cells) > 2 && strings.TrimSpace(cells[1]) == edit.Name {
				copy := l
				current = &copy
				break
			}
		}
		if current != nil && current.text == edit.After {
			continue
		}
		if current == nil && edit.After == "" {
			continue
		}
		if current == nil {
			if edit.Before != "" {
				return nil, ErrConflict
			}
			// Append within the table, before any prose below it.
			pos := -1
			for _, l := range lines(out) {
				if strings.HasPrefix(strings.TrimSpace(l.text), "|") {
					pos = l.end
				}
			}
			if pos < 0 {
				return nil, errors.New("tag registry has no markdown table")
			}
			addition := edit.After + newline(out)
			if pos > 0 && out[pos-1] != '\n' {
				addition = newline(out) + addition
			}
			out = append(append(append([]byte{}, out[:pos]...), []byte(addition)...), out[pos:]...)
			continue
		}
		if current.text != edit.Before {
			return nil, ErrConflict
		}
		replacement := []byte{}
		if edit.After != "" {
			replacement = append([]byte(edit.After), out[current.start+len(current.text):current.end]...)
		}
		out = append(append(append([]byte{}, out[:current.start]...), replacement...), out[current.end:]...)
	}
	return out, nil
}

// update rebases against the latest bytes and rechecks the hash just before
// atomic rename. os.Root rejects paths/symlinks escaping the vault.
func (v *Vault) update(path string, guard func() error, edit func([]byte) ([]byte, error)) error {
	if err := v.root.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		before, err := v.Read(path)
		if err != nil {
			return err
		}
		after, err := edit(before)
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			return nil
		}
		if err := guard(); err != nil {
			return err
		}
		mode := os.FileMode(0640)
		if info, err := v.root.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		suffix := make([]byte, 8)
		if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
			return err
		}
		temp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".scribble-%x.tmp", suffix))
		f, err := v.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = f.Write(after)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = v.root.Remove(temp)
			return err
		}
		latest, err := v.Read(path)
		if err != nil {
			_ = v.root.Remove(temp)
			return err
		}
		if sha256.Sum256(latest) != sha256.Sum256(before) {
			_ = v.root.Remove(temp)
			continue
		}
		if err := guard(); err != nil {
			_ = v.root.Remove(temp)
			return err
		}
		err = v.root.Rename(temp, path)
		if err != nil {
			_ = v.root.Remove(temp)
			return err
		}
		dir, err := v.root.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		_ = dir.Close()
		return err
	}
	return errors.New("vault is changing too quickly; retry later")
}
func (v *Vault) Apply(p Plan) error {
	today := func() error {
		if p.Date != v.settings.Today(v.now()) {
			return errors.New("past notes are read-only")
		}
		return nil
	}
	// Create exclusively, never truncate an existing daily note.
	b, err := v.Read(p.Path)
	if err != nil {
		return err
	}
	if b == nil {
		if err := today(); err != nil {
			return err
		}
		if err := v.root.MkdirAll(filepath.Dir(p.Path), 0750); err != nil {
			return err
		}
		f, err := v.root.OpenFile(p.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if err == nil {
			_, err = f.WriteString("## Scribbles\n")
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if err := v.update(p.Path, today, func(b []byte) ([]byte, error) { return EditNote(b, p) }); err != nil {
		return err
	}
	return v.update(TagsPath, func() error { return nil }, func(b []byte) ([]byte, error) { return EditTags(b, p.Tags) })
}
