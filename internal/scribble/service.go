package scribble

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type undoRequest struct {
	id     string
	result chan error
}
type Service struct {
	Config      Config
	Store       *Store
	Vault       *Vault
	Models      *Models
	Log         *slog.Logger
	Now         func() time.Time
	wake        chan struct{}
	undo        chan undoRequest
	mu          sync.Mutex
	subscribers map[chan struct{}]bool
}

func NewService(c Config, log *slog.Logger) (*Service, error) {
	store, err := OpenStore(c.QueueDB)
	if err != nil {
		return nil, err
	}
	vault, err := OpenVault(c, time.Now)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	return &Service{Config: c, Store: store, Vault: vault, Models: NewModels(c), Log: log, Now: time.Now, wake: make(chan struct{}, 1), undo: make(chan undoRequest), subscribers: map[chan struct{}]bool{}}, nil
}
func (s *Service) Close() error { _ = s.Vault.Close(); return s.Store.Close() }
func (s *Service) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.notify()
}
func (s *Service) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers[ch] = true
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.subscribers, ch); s.mu.Unlock() }
}
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		// Recover undo before processing any more captures. Both use this goroutine.
		pending, err := s.Store.UndoIntent()
		if err != nil {
			return err
		}
		if pending != nil {
			if err = s.applyUndo(*pending); err != nil {
				return fmt.Errorf("recover undo: %w", err)
			}
		}
		select {
		case req := <-s.undo:
			req.result <- s.performUndo(req.id)
			s.notify()
			continue
		default:
		}
		job, err := s.Store.Next()
		if err != nil {
			return err
		}
		if job != nil && job.NextAt <= s.Now().UnixMilli() {
			if err := s.process(ctx, job); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.Log.Warn("job.failed", "job_id", job.ID, "error", err, "attempt", job.Attempts+1)
				if err = s.Store.Fail(job, s.Config, err, s.Now()); err != nil {
					return err
				}
			}
			s.notify()
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case req := <-s.undo:
			req.result <- s.performUndo(req.id)
			s.notify()
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func decide(target *Entry, p float64, today string, c JevConfig) string {
	if target == nil {
		return "new"
	}
	if target.Date == today {
		if p >= c.AmendConfidence {
			return "amend"
		}
		return "link"
	}
	if p >= c.LinkConfidence {
		return "link"
	}
	return "new"
}
func (s *Service) process(ctx context.Context, j *Job) error {
	if j.Plan != nil && j.Plan.Date != s.Config.today(s.Now()) {
		raw, err := s.Vault.read(j.Plan.Path)
		if err != nil {
			return err
		}
		line, found, err := findEntry(raw, j.Plan.Block)
		if err != nil {
			return err
		}
		// An intent that never reached the note can be routed again on the new
		// processing day. Already-applied intents only finish their bookkeeping.
		unapplied := (!found && j.Plan.Before == "") || (found && line.text == j.Plan.Before)
		if unapplied {
			if err := s.Store.ClearPlan(j.ID); err != nil {
				return err
			}
			j.Plan = nil
		}
	}
	if j.Plan != nil {
		if err := s.Vault.Apply(*j.Plan); err != nil {
			return err
		}
		return s.Store.Complete(j.ID, *j.Plan, s.Now())
	}
	if len(j.Audio) > 0 && j.Text == "" {
		if err := s.Store.State(j.ID, "transcribing"); err != nil {
			return err
		}
		s.notify()
		text, err := s.Models.Transcribe(ctx, j)
		if err != nil {
			return err
		}
		j.Text = text
		if err = s.Store.Transcribed(j.ID, text); err != nil {
			return err
		}
	}
	if err := s.Store.State(j.ID, "routing"); err != nil {
		return err
	}
	s.notify()
	entries, registry, err := s.Vault.Context()
	if err != nil {
		return err
	}
	last, err := s.Store.Last()
	if err != nil {
		return err
	}
	route, err := s.Models.Route(ctx, j.Text, entries, registry, last)
	if err != nil {
		return err
	}
	targetID := route.Target
	if targetID == "last" {
		targetID = "none"
		if last != nil && !last.Undone && last.Plan != nil {
			targetID = last.Plan.Path + "#^" + last.Plan.Block
		}
	}
	var target *Entry
	for i := range entries {
		if entries[i].ID == targetID {
			target = &entries[i]
			break
		}
	}
	if target == nil && last != nil && !last.Undone && last.Plan != nil && targetID == last.Plan.Path+"#^"+last.Plan.Block {
		raw, e := s.Vault.read(last.Plan.Path)
		if e != nil {
			return e
		}
		line, found, e := findEntry(raw, last.Plan.Block)
		if e != nil {
			return e
		}
		if found {
			target = &Entry{ID: targetID, Block: last.Plan.Block, Path: last.Plan.Path, Date: last.Date, Line: line.text}
		}
	}
	if targetID != "none" && target == nil {
		return errors.New("routing: Jev selected an unavailable target")
	}
	today := s.Config.today(s.Now())
	action := decide(target, route.Probability, today, s.Config.Jev)
	tags := []string{}
	for name, p := range route.Tags {
		if p >= s.Config.Jev.TagConfidence {
			tags = append(tags, name)
		}
	}
	sort.Strings(tags)
	newTag := route.NewTag >= s.Config.Jev.TagConfidence
	text := j.Text
	var created []Tag
	var claudeLatency time.Duration
	called := action == "amend" || newTag
	if called {
		var amend *Entry
		if action == "amend" {
			amend = target
		}
		writing, latency, err := s.Models.Write(ctx, j.Text, amend, tags, registry, newTag)
		claudeLatency = latency
		if err != nil {
			return err
		}
		if amend != nil {
			text = strings.TrimSpace(writing.Text)
			tags = append(tags, writing.Tags...)
		}
		created = writing.NewTags
		for _, t := range created {
			tags = append(tags, t.Name)
		}
	}
	// The registry is user-editable. Re-read it after all model calls, before
	// preparing the write; removed/renamed tags are never resurrected by routing.
	raw, err := s.Vault.read(tagsPath)
	if err != nil {
		return err
	}
	fresh, err := parseTags(raw)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, t := range fresh {
		allowed[t.Name] = true
	}
	for _, t := range created {
		allowed[t.Name] = true
	}
	if action == "amend" {
		for _, t := range tagsIn(target.Line) {
			allowed[t] = true
		}
	}
	unique := map[string]bool{}
	finalTags := []string{}
	for _, tag := range tags {
		if allowed[tag] && !unique[tag] {
			unique[tag] = true
			finalTags = append(finalTags, tag)
		}
	}
	sort.Strings(finalTags)
	if action == "link" {
		text += " (see [[" + strings.TrimSuffix(filepath.Base(target.Path), ".md") + "#^" + target.Block + "]])"
	}
	path, err := s.Vault.dailyPath(s.Now())
	if err != nil {
		return err
	}
	raw, err = s.Vault.read(path)
	if err != nil {
		return err
	}
	block, err := newBlock(raw)
	if err != nil {
		return err
	}
	before := ""
	if action == "amend" {
		path = target.Path
		block = target.Block
		before = target.Line
	}
	// A model response crossing midnight must be re-routed against the new day.
	if today != s.Config.today(s.Now()) {
		return errors.New("day changed during processing; retrying with today's context")
	}
	line := "- " + normalizeInput(text)
	if len(finalTags) > 0 {
		line += " " + strings.Join(finalTags, " ")
	}
	line += " ^" + block
	p := Plan{Date: today, Path: path, Block: block, Before: before, After: line, Action: action, Tags: makeTagEdits(fresh, tagsIn(line), created, today)}
	// Validate both edits before committing either, and persist the intent first.
	raw, err = s.Vault.read(path)
	if err != nil {
		return err
	}
	if _, err = editNote(raw, p); err != nil {
		return err
	}
	raw, err = s.Vault.read(tagsPath)
	if err != nil {
		return err
	}
	if _, err = editTags(raw, p.Tags); err != nil {
		return err
	}
	if err = s.Store.Prepare(j.ID, p); err != nil {
		return err
	}
	s.Log.Info("route.decision", "job_id", j.ID, "captured_at", j.CapturedAt, "input", j.Text, "target_choice", route.Target, "target_probability", route.Probability, "tag_probabilities", route.Tags, "new_tag_probability", route.NewTag, "thresholds", map[string]float64{"amend_confidence": s.Config.Jev.AmendConfidence, "link_confidence": s.Config.Jev.LinkConfidence, "tag_confidence": s.Config.Jev.TagConfidence}, "action", action, "target_id", targetID, "tags", finalTags, "new_tags", created, "claude_called", called, "jev_latency_ms", route.Latency.Milliseconds(), "claude_latency_ms", claudeLatency.Milliseconds())
	if err = s.Vault.Apply(p); err != nil {
		return err
	}
	return s.Store.Complete(j.ID, p, s.Now())
}
func (s *Service) Undo(ctx context.Context, id string) error {
	req := undoRequest{id: id, result: make(chan error, 1)}
	select {
	case s.undo <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) performUndo(id string) error {
	last, err := s.Store.Last()
	if err != nil {
		return err
	}
	if last == nil || last.ID != id {
		return errors.New("undo only applies to the most recent completed write")
	}
	if last.Undone {
		return nil
	}
	if last.Date != s.Config.today(s.Now()) {
		return errors.New("undo only works on today's note")
	}
	p := *last.Plan
	p.Before, p.After = p.After, p.Before
	p.Undo = true
	p.Tags = nil
	raw, err := s.Vault.read(p.Path)
	if err != nil {
		return err
	}
	if _, err = editNote(raw, p); err != nil {
		return err
	}
	intent := UndoIntent{JobID: id, Plan: p}
	if err = s.Store.PrepareUndo(intent); err != nil {
		return err
	}
	return s.applyUndo(intent)
}
func (s *Service) applyUndo(intent UndoIntent) error {
	if err := s.Vault.Apply(intent.Plan); err != nil {
		return err
	}
	if err := s.Store.CompleteUndo(intent.JobID); err != nil {
		return err
	}
	s.Log.Info("entry.undo", "job_id", intent.JobID)
	return nil
}
