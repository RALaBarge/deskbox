package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRequeueNeverBlocks guards the deadlock a review turned up: workers
// used to re-enqueue their own retries with a direct send on q.ch, so once
// ch filled (256) with every worker stuck in that same send, nothing was
// left to drain it — permanent deadlock. Enqueueing far past ch's capacity
// with nothing receiving must now return promptly instead of blocking; the
// backlog absorbs it and the dispatcher hands it off as workers free up.
func TestRequeueNeverBlocks(t *testing.T) {
	q := NewQueue(1, nil) // deliberately NOT started: nothing drains ch
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ { // ~4x ch's capacity
			if err := q.requeue(&Job{ID: "job-backlog", Status: StatusQueued}); err != nil {
				t.Errorf("requeue %d failed: %v", i, err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("requeue blocked past ch capacity — the retry-path deadlock is back")
	}
}

// TestEnqueueRefusesWhenFull checks the other half: admission is capped, so
// a runaway submitter gets a fast, visible refusal instead of unbounded
// backlog growth.
func TestEnqueueRefusesWhenFull(t *testing.T) {
	q := NewQueue(1, nil)
	for i := 0; i < maxPending; i++ {
		if err := q.reserve(); err != nil {
			t.Fatalf("reserve %d should have been admitted: %v", i, err)
		}
		q.enqueueReserved(&Job{ID: "job-fill", Status: StatusQueued})
	}
	if err := q.reserve(); !errors.Is(err, errQueueFull) {
		t.Fatalf("expected errQueueFull past the cap, got %v", err)
	}
	// Already-accepted work still gets through — the cap is admission-only.
	if err := q.requeue(&Job{ID: "job-retry", Status: StatusQueued}); err != nil {
		t.Fatalf("requeue must bypass the admission cap, got %v", err)
	}
}

// TestOutstandingReservationsCountAgainstCap guards the overshoot a burst of
// concurrent submits could otherwise cause: each has reserved a slot but not
// yet filled it, and a cap that only counted len(pending) would admit all of
// them.
func TestReservationsCountAgainstCap(t *testing.T) {
	q := NewQueue(1, nil)
	for i := 0; i < maxPending; i++ {
		if err := q.reserve(); err != nil {
			t.Fatalf("reserve %d should have been admitted: %v", i, err)
		}
	}
	if err := q.reserve(); !errors.Is(err, errQueueFull) {
		t.Fatalf("outstanding reservations must count against the cap, got %v", err)
	}
	q.release()
	if err := q.reserve(); err != nil {
		t.Fatalf("a released reservation must free its slot, got %v", err)
	}
}

// readingStore is the point of these tests. An earlier version of the race
// test used NewQueue(..., nil), where persist() short-circuits on a nil
// store and never touches the job outside the lock — so it passed happily
// with the bug reverted and guarded nothing at all. A store that actually
// reads the *Job it is handed, the way a real one does, is what makes the
// unsynchronised access observable. The sleep widens the window so the
// detector sees it every run rather than occasionally.
type readingStore struct {
	JobStore // nil: these tests only ever call Update
	sink     atomic.Value
}

func (s *readingStore) Update(job *Job) error {
	time.Sleep(2 * time.Millisecond)
	s.sink.Store(fmt.Sprint(job.Status, job.Acked, job.Attempt, job.Error, job.Finished))
	return nil
}

func (s *readingStore) Ack(string, time.Time) error { return nil }

// TestAckFinishRaceIsGuarded covers the bug a review found: writes to a
// job's fields under the mutex racing an unsynchronised read of the same
// pointer inside store.Update. Reverting any of the copy-under-lock changes
// in finish/finishCanceled/process must make this fail under -race.
func TestAckFinishRaceIsGuarded(t *testing.T) {
	q := NewQueue(1, &readingStore{})
	job := &Job{ID: "job-race-test", Status: StatusRunning, Created: time.Now().UTC()}
	q.mu.Lock()
	q.jobs[job.ID] = job
	q.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		q.finish(job, map[string]any{"ok": true}, nil)
	}()
	go func() {
		defer wg.Done()
		// Ack only succeeds once the job is terminal; poll until finish()
		// gets there, or give up after a bound so a genuine bug (Ack never
		// succeeding) fails the test instead of hanging it.
		for i := 0; i < 10000; i++ {
			if _, err := q.Ack(job.ID); err == nil {
				return
			}
			time.Sleep(time.Microsecond)
		}
		t.Error("Ack never succeeded against the finishing job")
	}()
	wg.Wait()

	q.mu.Lock()
	acked := job.Acked
	q.mu.Unlock()
	if !acked {
		t.Error("expected job to be acked after Ack() returned nil error")
	}
}

// TestCancelDuringRunningPersistIsGuarded covers the half the first fix
// missed: process() persisted the live pointer on its running and retry
// transitions while Cancel() wrote Status/Finished under the lock.
//
// This drives the real process() rather than re-implementing its steps.
// An earlier draft mirrored the transition inline and "passed" with the
// production fix reverted — it was testing its own copy of the logic,
// which is the same worthlessness as testing against a nil store.
func TestCancelDuringRunningPersistIsGuarded(t *testing.T) {
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "tools", "slow")
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(toolDir, "tcs.yaml"), `
name: slow
input: {type: object}
output: {schema: {type: object}}
allowed_side_effects: {files: [], network: false}
execution: {mode: queued, max_retries: 0, timeout_ms: 20000}
`)
	writeFile(t, filepath.Join(toolDir, "run.sh"), "#!/bin/sh\nsleep 1\necho '{\"ok\":true}'\n")
	if err := os.Chmod(filepath.Join(toolDir, "run.sh"), 0o700); err != nil {
		t.Fatal(err)
	}

	tools, err := LoadTools(filepath.Join(dir, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	tool := tools["slow"]
	if tool == nil {
		t.Fatal("tool did not load")
	}

	q := NewQueue(1, &readingStore{})
	NewDesk(tools, q, filepath.Join(dir, "data"),
		&Settings{JobMemoryMax: "512M", JobTasksMax: 64}, false, false)

	job := &Job{ID: "job-cancel-race", Tool: "slow", Input: map[string]any{},
		Status: StatusQueued, Created: time.Now().UTC()}
	q.mu.Lock()
	q.jobs[job.ID] = job
	q.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); q.process(job) }()
	go func() {
		defer wg.Done()
		// Land inside the running window: after process() has flipped the
		// job to running and started persisting, while the tool still runs.
		time.Sleep(150 * time.Millisecond)
		q.Cancel(job.ID)
	}()
	wg.Wait()
}

// countingStore records whether Insert was reached at all.
type countingStore struct {
	JobStore
	inserts atomic.Int64
}

func (s *countingStore) Insert(job *Job, key string) (*Job, bool, error) {
	s.inserts.Add(1)
	return job, true, nil
}
func (s *countingStore) Update(*Job) error { return nil }

// TestRefusedSubmitNeverPersists guards a bug a review found: the admission
// cap was checked *after* store.Insert and after the job entered q.jobs, so
// a 503'd submit left a row behind that nothing would ever run. Resume()
// picked it up on the next restart despite the caller being told it was
// refused, GET /jobs/{id} showed it queued forever, and a retry with the
// same idempotency_key deduped against that orphan — returning 202 for a
// job that could never run, with /wait hanging to its deadline.
func TestRefusedSubmitNeverPersists(t *testing.T) {
	store := &countingStore{}
	q := NewQueue(1, store)
	for i := 0; i < maxPending; i++ {
		if err := q.reserve(); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		q.enqueueReserved(&Job{ID: "filler", Status: StatusQueued})
	}

	tool := &Tool{Name: "t", Execution: ExecutionSpec{}}
	_, err := q.Submit(tool, map[string]any{}, nil, "key-that-must-not-be-burned")
	if !errors.Is(err, errQueueFull) {
		t.Fatalf("expected errQueueFull, got %v", err)
	}
	if n := store.inserts.Load(); n != 0 {
		t.Fatalf("a refused submit must not touch the store, but Insert ran %d time(s) — "+
			"the row it wrote would orphan the job and burn its idempotency key", n)
	}
	q.mu.Lock()
	tracked := len(q.jobs)
	q.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("a refused submit must not land in q.jobs, found %d", tracked)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
