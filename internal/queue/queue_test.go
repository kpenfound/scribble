package queue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/scribble/internal/vault"
)

func testDB(t *testing.T) string { return filepath.Join(t.TempDir(), "queue.db") }

func testVaultSettings(t *testing.T) vault.Settings {
	t.Helper()
	loc, err := time.LoadLocation("America/Detroit")
	if err != nil {
		t.Fatal(err)
	}
	layout, err := vault.MomentLayout("YYYY-MM-DD")
	if err != nil {
		t.Fatal(err)
	}
	return vault.Settings{Dir: t.TempDir(), DailyDir: "Daily", Layout: layout, Location: loc, LookbackDays: 7}
}

var testPolicy = RetryPolicy{MaxAttempts: 3, Backoff: 5 * time.Second}

func TestDurableQueueFIFOAndRecovery(t *testing.T) {
	db := testDB(t)
	s, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(db); err == nil {
		t.Fatal("second writer acquired queue")
	}
	a, _ := s.Submit(Job{ID: "a", Text: "first", Audio: []byte{1, 2}, MIME: "audio/webm"})
	_, _ = s.Submit(Job{ID: "b", Text: "second"})
	duplicate, _ := s.Submit(Job{ID: "a", Text: "changed"})
	if duplicate.Text != "first" {
		t.Fatal("retry overwrote payload")
	}
	if err = s.State(a.ID, "routing"); err != nil {
		t.Fatal(err)
	}
	if err = s.Fail(a, testPolicy, errors.New("temporary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Next()
	if next.ID != "a" || next.NextAt <= time.Now().UnixMilli() {
		t.Fatal("FIFO skipped retry backoff")
	}
	_ = s.Close()
	s, err = OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next, _ = s.Next()
	if next.ID != "a" || len(next.Audio) != 2 || next.Attempts != 1 {
		t.Fatalf("lost persisted queue: %+v", next)
	}
	for i := 1; i < testPolicy.MaxAttempts; i++ {
		if err = s.Fail(next, testPolicy, errors.New("failed"), time.Now()); err != nil {
			t.Fatal(err)
		}
		next, _ = s.Get("a")
	}
	next, _ = s.Next()
	if next.ID != "b" {
		t.Fatal("terminal failure blocks queue")
	}
	if err = s.Retry("a"); err != nil {
		t.Fatal(err)
	}
	next, _ = s.Next()
	if next.ID != "b" {
		t.Fatal("manual retry jumped the queue")
	}
}
func TestRestartRestoresInFlightVoiceAndUndoJournal(t *testing.T) {
	db, c := testDB(t), testVaultSettings(t)
	store, err := OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := store.Submit(Job{ID: "inflight", Audio: []byte("voice"), MIME: "audio/webm"})
	if err = store.State(j.ID, "transcribing"); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	j, _ = store.Next()
	if j.State != "queued" || string(j.Audio) != "voice" {
		t.Fatalf("lost in-flight voice: %+v", j)
	}
	now := time.Now()
	v, err := vault.Open(c, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	path, _ := v.DailyPath(now)
	p := vault.Plan{Date: c.Today(now), Path: path, Block: "scr-test", After: "- crash test ^scr-test", Action: "new"}
	if err = store.Prepare(j.ID, p); err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	if err = store.Complete(j.ID, p, now); err != nil {
		t.Fatal(err)
	}
	p.Before, p.After = p.After, ""
	p.Undo = true
	intent := UndoIntent{JobID: j.ID, Plan: p}
	if err = store.PrepareUndo(intent); err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	// Death after the inverse rename but before recording undone must be safe.
	recovered, err := store.UndoIntent()
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(recovered.Plan); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteUndo(j.ID); err != nil {
		t.Fatal(err)
	}
	j, _ = store.Get(j.ID)
	if !j.Undone || strings.Contains(readFixture(t, c.Dir, path), "crash test") {
		t.Fatal("undo recovery failed")
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
