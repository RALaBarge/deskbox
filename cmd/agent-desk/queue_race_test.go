package main

import (
	"errors"
	"sync"
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
		if err := q.enqueue(&Job{ID: "job-fill", Status: StatusQueued}); err != nil {
			t.Fatalf("enqueue %d should have been admitted: %v", i, err)
		}
	}
	if err := q.enqueue(&Job{ID: "job-over", Status: StatusQueued}); !errors.Is(err, errQueueFull) {
		t.Fatalf("expected errQueueFull past the cap, got %v", err)
	}
	// Already-accepted work still gets through — the cap is admission-only.
	if err := q.requeue(&Job{ID: "job-retry", Status: StatusQueued}); err != nil {
		t.Fatalf("requeue must bypass the admission cap, got %v", err)
	}
}

// TestAckFinishNoRace guards the bug a review turned up: finish/
// finishCanceled used to persist the shared *Job pointer directly, so a
// concurrent Ack() racing the same job (Ack requires the job to already be
// terminal, so the window is "just after finish sets Status, before it
// returns") could read/write job.Acked at the same time finish read the
// pointer's fields for its own store.Update call — a real data race, and,
// separately, a real chance for the DB to end up with acked=false forever
// even though the operator's Ack() succeeded in memory. Run with -race.
func TestAckFinishNoRace(t *testing.T) {
	q := NewQueue(1, nil)
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
