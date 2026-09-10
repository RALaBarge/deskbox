package main

import (
	"sync"
	"testing"
	"time"
)

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
