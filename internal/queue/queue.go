// Package queue is the durable, FIFO job queue backed by SQLite.
package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"github.com/kpenfound/scribble/internal/vault"
)

type Job struct {
	ID         string      `json:"id"`
	CapturedAt string      `json:"captured_at"`
	Text       string      `json:"-"`
	Audio      []byte      `json:"-"`
	MIME       string      `json:"-"`
	State      string      `json:"state"`
	Attempts   int         `json:"attempts"`
	NextAt     int64       `json:"-"`
	Error      string      `json:"error,omitempty"`
	Plan       *vault.Plan `json:"-"`
	Line       string      `json:"line,omitempty"`
	Previous   string      `json:"previous,omitempty"`
	Action     string      `json:"action,omitempty"`
	WrittenAt  string      `json:"written_at,omitempty"`
	Date       string      `json:"date,omitempty"`
	Undone     bool        `json:"undone"`
	Sequence   int64       `json:"sequence"`
}
type Store struct {
	db   *sql.DB
	lock *os.File
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, errors.New("another scribble process owns this queue")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS jobs (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, captured_at TEXT NOT NULL,
 text TEXT NOT NULL, audio BLOB, mime TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'queued',
 attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '', plan TEXT NOT NULL DEFAULT '', line TEXT NOT NULL DEFAULT '',
 previous TEXT NOT NULL DEFAULT '', action TEXT NOT NULL DEFAULT '', written_at TEXT NOT NULL DEFAULT '',
 date TEXT NOT NULL DEFAULT '', undone INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
 UPDATE jobs SET state='queued' WHERE state IN ('transcribing','routing');`)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	err := s.db.Close()
	_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
	_ = s.lock.Close()
	return err
}

const jobColumns = `id,captured_at,text,audio,mime,state,attempts,next_at,error,plan,line,previous,action,written_at,date,undone,seq`

func scanJob(row interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	var plan string
	err := row.Scan(&j.ID, &j.CapturedAt, &j.Text, &j.Audio, &j.MIME, &j.State, &j.Attempts, &j.NextAt, &j.Error, &plan, &j.Line, &j.Previous, &j.Action, &j.WrittenAt, &j.Date, &j.Undone, &j.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plan != "" {
		j.Plan = &vault.Plan{}
		if err = json.Unmarshal([]byte(plan), j.Plan); err != nil {
			return nil, err
		}
	}
	return j, nil
}
func (s *Store) Submit(j Job) (*Job, error) {
	_, err := s.db.Exec(`INSERT INTO jobs(id,captured_at,text,audio,mime) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, j.ID, j.CapturedAt, j.Text, j.Audio, j.MIME)
	if err != nil {
		return nil, err
	}
	return s.Get(j.ID)
}
func (s *Store) Get(id string) (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}
func (s *Store) Next() (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT ` + jobColumns + ` FROM jobs WHERE state IN ('queued','transcribing','routing') ORDER BY seq LIMIT 1`))
}
func (s *Store) Last() (*Job, error) {
	return scanJob(s.db.QueryRow(`SELECT ` + jobColumns + ` FROM jobs WHERE state='written' ORDER BY seq DESC LIMIT 1`))
}
func (s *Store) Status() ([]Job, error) {
	rows, err := s.db.Query(`SELECT ` + strings.Replace(jobColumns, ",audio,", ",NULL,", 1) + ` FROM jobs WHERE state!='written' OR seq=(SELECT MAX(seq) FROM jobs WHERE state='written') ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *j)
	}
	return jobs, rows.Err()
}
func (s *Store) State(id, state string) error {
	_, err := s.db.Exec(`UPDATE jobs SET state=? WHERE id=?`, state, id)
	return err
}
func (s *Store) Transcribed(id, text string) error {
	_, err := s.db.Exec(`UPDATE jobs SET text=? WHERE id=?`, text, id)
	return err
}
func (s *Store) Prepare(id string, p vault.Plan) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE jobs SET plan=? WHERE id=?`, string(b), id)
	return err
}
func (s *Store) Complete(id string, p vault.Plan, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET state='written',line=?,previous=?,action=?,written_at=?,date=?,error='',audio=NULL WHERE id=?`, p.After, p.Before, p.Action, now.Format(time.RFC3339), p.Date, id)
	return err
}

// RetryPolicy bounds automatic attempts and sets the first backoff step.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

func (s *Store) Fail(j *Job, c RetryPolicy, problem error, now time.Time) error {
	attempts := j.Attempts + 1
	state := "queued"
	if attempts >= c.MaxAttempts {
		state = "failed"
	}
	delay := c.Backoff * time.Duration(1<<min(attempts-1, 8))
	_, err := s.db.Exec(`UPDATE jobs SET attempts=?,state=?,next_at=?,error=? WHERE id=?`, attempts, state, now.Add(delay).UnixMilli(), problem.Error(), j.ID)
	return err
}
func (s *Store) Retry(id string) error {
	// Changing the sequence places a manual retry after already queued captures.
	result, err := s.db.Exec(`UPDATE jobs SET seq=(SELECT COALESCE(MAX(seq),0)+1 FROM jobs),state='queued',attempts=0,next_at=0,error='' WHERE id=? AND state='failed'`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("only failed jobs can be retried")
	}
	return nil
}

type UndoIntent struct {
	JobID string
	Plan  vault.Plan
}

func (s *Store) UndoIntent() (*UndoIntent, error) {
	var b string
	err := s.db.QueryRow(`SELECT value FROM metadata WHERE key='undo'`).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p UndoIntent
	err = json.Unmarshal([]byte(b), &p)
	return &p, err
}
func (s *Store) PrepareUndo(p UndoIntent) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO metadata(key,value) VALUES('undo',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(b))
	return err
}
func (s *Store) CompleteUndo(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE jobs SET undone=1 WHERE id=?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM metadata WHERE key='undo'`); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ClearPlan(id string) error {
	_, err := s.db.Exec(`UPDATE jobs SET plan='' WHERE id=?`, id)
	return err
}

func (s *Store) Check() error {
	var result string
	if err := s.db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite: %s", result)
	}
	return nil
}

// Input limits and normalisation shared by the HTTP layer and transcription.
const MaxText = 16 * 1024

// NormalizeText collapses newlines to spaces and trims the result.
func NormalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}
