package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

type JobStatus string

const (
	StatusQueued   JobStatus = "queued"
	StatusRunning  JobStatus = "running"
	StatusDone     JobStatus = "done"
	StatusFailed   JobStatus = "failed"
	StatusCanceled JobStatus = "canceled"
)

// isTerminal reports whether a job has reached a state that will never
// change again — nothing further will run for it, and no future write
// (retry, cancel) can move it out of this status.
func (s JobStatus) isTerminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Job is one tool invocation flowing through the desk. Job IDs are what the
// agent polls; the desk keeps the history in memory so the contract story is
// auditable ("what did the agent try, and why did it not conform?").
type Job struct {
	ID             string `json:"id"`
	Tool           string `json:"tool"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// BatchID is set only for a job created via SubmitBatch — every item in
	// one POST /batches call shares it, so GET /batches/{id} can find them
	// all. Empty for an ordinary single POST /tools/{name} submit.
	BatchID    string         `json:"batch_id,omitempty"`
	Input      map[string]any `json:"input"`
	Meta       map[string]any `json:"meta,omitempty"`
	Status     JobStatus      `json:"status"`
	Attempt    int            `json:"attempt"`
	MaxRetries int            `json:"max_retries"`
	Result     any            `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
	Created    time.Time      `json:"created"`
	Started    *time.Time     `json:"started,omitempty"`
	Finished   *time.Time     `json:"finished,omitempty"`
	// Acked marks a terminal job's result as seen/handled by the operator —
	// the inbox model (GET /jobs lists terminal+unacked, POST
	// /jobs/{id}/ack clears one) that replaces claim/TTL-based delivery:
	// with one desk process instead of many independent ones, multiple
	// readers seeing the same unacked job is harmless, so this is a plain
	// flag, not a claim.
	Acked   bool       `json:"acked"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

type Queue struct {
	mu      sync.Mutex
	desk    *Desk
	jobs    map[string]*Job
	ch      chan *Job
	workers int
	stop    chan struct{}
	wg      sync.WaitGroup
	store   JobStore // optional: nil means in-memory only, no idempotency across restarts
	// cancels holds the cancel func for every job currently *running* (not
	// queued) — set right before Execute is called, deleted right after it
	// returns. A queued job (sitting in ch, not yet picked up by a worker)
	// has no entry here; Cancel() handles that case by flipping its status
	// directly, and process() checks for that the moment it dequeues.
	cancels map[string]context.CancelFunc
}

func NewQueue(workers int, store JobStore) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{
		jobs:    map[string]*Job{},
		ch:      make(chan *Job, 256),
		workers: workers,
		stop:    make(chan struct{}),
		store:   store,
		cancels: map[string]context.CancelFunc{},
	}
}

// Resume reloads jobs left "queued" or "running" by a prior process
// lifetime and re-enqueues them, so a desk restart resumes work instead of
// silently dropping it. No-op when no store is configured.
func (q *Queue) Resume() (int, error) {
	if q.store == nil {
		return 0, nil
	}
	pending, err := q.store.LoadIncomplete()
	if err != nil {
		return 0, err
	}
	for _, job := range pending {
		job.Status = StatusQueued
		q.mu.Lock()
		q.jobs[job.ID] = job
		q.mu.Unlock()
		select {
		case q.ch <- job:
		case <-q.stop:
			return len(pending), errors.New("queue stopped during resume")
		}
	}
	return len(pending), nil
}

func (q *Queue) Start() {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

func (q *Queue) Stop() {
	close(q.stop)
	q.wg.Wait()
}

func (q *Queue) Submit(tool *Tool, input, meta map[string]any, idempotencyKey string) (*Job, error) {
	return q.submit(tool, input, meta, idempotencyKey, "")
}

// SubmitBatch fans out one job per item under a shared batch id — the
// first-class replacement for lease-based chunk claiming. There is one
// worker (the desk itself), and jobs already resume via Resume/
// LoadIncomplete after a crash, so "what happens if the worker dies
// mid-batch" is exactly the existing per-job lifecycle: nothing new to
// invent for a lease to expire out of. No idempotency dedup per item —
// each item is its own fresh job.
func (q *Queue) SubmitBatch(tool *Tool, items []map[string]any, meta map[string]any) (string, []*Job, error) {
	batchID := newBatchID()
	jobs := make([]*Job, 0, len(items))
	for _, item := range items {
		job, err := q.submit(tool, item, meta, "", batchID)
		if err != nil {
			return batchID, jobs, err
		}
		jobs = append(jobs, job)
	}
	return batchID, jobs, nil
}

func (q *Queue) submit(tool *Tool, input, meta map[string]any, idempotencyKey, batchID string) (*Job, error) {
	job := &Job{
		ID: newJobID(), Tool: tool.Name, IdempotencyKey: idempotencyKey,
		BatchID: batchID, Input: input, Meta: meta,
		Status: StatusQueued, MaxRetries: tool.Execution.MaxRetries,
		Created: time.Now().UTC(),
	}

	if q.store != nil {
		existing, created, err := q.store.Insert(job, idempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("persist job: %w", err)
		}
		if !created {
			// Same (tool, idempotency_key) already has a job. If it's still
			// tracked in-process (queued/running), that pointer — not the
			// point-in-time DB snapshot — is the live truth: overwriting the
			// map entry with the snapshot here would orphan it, and every
			// later GET would show the job frozen at whatever status it had
			// at this exact moment, even after it actually finishes.
			q.mu.Lock()
			live, tracked := q.jobs[existing.ID]
			if !tracked {
				q.jobs[existing.ID] = existing
				live = existing
			}
			cp := *live
			q.mu.Unlock()
			return &cp, nil
		}
		job = existing // canonical row as the store persisted it
	}

	q.mu.Lock()
	q.jobs[job.ID] = job
	q.mu.Unlock()
	select {
	case q.ch <- job:
		return q.snapshot(job), nil
	case <-q.stop:
		return nil, errors.New("queue stopped")
	}
}

// snapshot copies a Job's current field values under the queue's lock. A
// worker mutates a Job's fields in place via its own pointer in q.jobs; any
// caller serializing that job for an HTTP response must not read it without
// the same lock, or it races the worker. Copying returns a safe, private
// view instead.
func (q *Queue) snapshot(job *Job) *Job {
	q.mu.Lock()
	cp := *job
	q.mu.Unlock()
	return &cp
}

// Get looks up a job. The error return is non-nil only for an actual store
// failure (e.g. the store unreachable) — callers must not fold that into "not
// found": an agent polling a real job during a DB blip needs to see that the
// lookup failed, not that its job vanished.
func (q *Queue) Get(id string) (*Job, bool, error) {
	q.mu.Lock()
	j, ok := q.jobs[id]
	q.mu.Unlock()
	if ok {
		return q.snapshot(j), true, nil
	}
	if q.store == nil {
		return nil, false, nil
	}
	stored, found, err := q.store.Get(id)
	if err != nil {
		log.Printf("job %s: lookup in store failed: %v", id, err)
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	q.mu.Lock()
	q.jobs[stored.ID] = stored
	q.mu.Unlock()
	return q.snapshot(stored), true, nil
}

func (q *Queue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.stop:
			return
		case job := <-q.ch:
			q.process(job)
		}
	}
}

func (q *Queue) process(job *Job) {
	tool := q.desk.tools[job.Tool]
	if tool == nil {
		q.finish(job, nil, errors.New("tool not found"))
		return
	}

	// Check-and-transition to Running, and register this attempt's cancel
	// func, as one atomic critical section. Closing the gap between "am I
	// canceled" and "I'm now running, here's how to cancel me" matters: a
	// Cancel() landing in that gap would otherwise see neither a queued job
	// it could flip directly nor a registered cancel func to call, and the
	// tool would run to completion despite being canceled.
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now().UTC()
	q.mu.Lock()
	if job.Status == StatusCanceled {
		q.mu.Unlock()
		cancel()
		q.finishCanceled(job)
		return
	}
	job.Status = StatusRunning
	job.Attempt++
	job.Started = &now
	q.cancels[job.ID] = cancel
	q.mu.Unlock()
	q.persist(job)

	result, err := q.desk.Execute(ctx, tool, job, job.Input)

	q.mu.Lock()
	delete(q.cancels, job.ID)
	canceled := job.Status == StatusCanceled
	q.mu.Unlock()
	cancel() // release the context's resources either way

	if canceled {
		q.finishCanceled(job)
		return
	}

	switch {
	case err == nil:
		q.finish(job, result, nil)
	case errors.Is(err, ErrContract):
		// Permanent: the agent's request (or the tool) violated the contract.
		// No retry — the agent must fix its call, not hope the desk goes easy.
		q.finish(job, nil, err)
	case job.Attempt < job.MaxRetries:
		backoff := time.Duration(1<<max(0, job.Attempt-1)) * 100 * time.Millisecond
		log.Printf("job %s tool %s attempt %d failed (retryable): %v", job.ID, job.Tool, job.Attempt, err)
		q.mu.Lock()
		job.Status = StatusQueued
		job.Error = err.Error()
		q.mu.Unlock()
		q.persist(job)
		select {
		case <-time.After(backoff):
		case <-q.stop:
			q.finish(job, nil, err)
			return
		}
		// A cancel could have landed during the backoff sleep, while the job
		// was sitting at StatusQueued with no registered cancel func (the
		// previous attempt's was already deleted above). Check before
		// re-enqueueing so a cancel requested mid-backoff isn't lost.
		q.mu.Lock()
		alreadyCanceled := job.Status == StatusCanceled
		q.mu.Unlock()
		if alreadyCanceled {
			q.finishCanceled(job)
			return
		}
		select {
		case q.ch <- job:
		case <-q.stop:
			q.finish(job, nil, err)
		}
	default:
		log.Printf("job %s tool %s failed after %d attempt(s): %v", job.ID, job.Tool, job.Attempt, err)
		q.finish(job, nil, err)
	}
}

func (q *Queue) finish(job *Job, result any, err error) {
	now := time.Now().UTC()
	q.mu.Lock()
	job.Finished = &now
	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
	} else {
		job.Status = StatusDone
		job.Result = result
	}
	q.mu.Unlock()
	q.persist(job)
}

// finishCanceled records a job an explicit Cancel() call already marked
// Canceled. It mirrors finish but never lets a done/failed result overwrite
// the Canceled status — whatever the tool's process actually returned (if
// it got a chance to return anything at all) is discarded.
func (q *Queue) finishCanceled(job *Job) {
	now := time.Now().UTC()
	q.mu.Lock()
	job.Finished = &now
	job.Error = "canceled by operator"
	q.mu.Unlock()
	q.persist(job)
}

// Cancel marks a queued or running job Canceled. A queued job (still
// sitting in ch, not yet picked up by a worker) is skipped the moment its
// worker turn comes; a running job's process is killed via the context
// Execute is running under. Returns (job, false, nil) if the job exists but
// already reached a terminal state — nothing to cancel, not an error. A
// job unknown to this process entirely (not tracked in memory, and either
// no store configured or not found in it) returns (nil, false, nil).
func (q *Queue) Cancel(id string) (*Job, bool, error) {
	q.mu.Lock()
	job, tracked := q.jobs[id]
	if tracked {
		if job.Status.isTerminal() {
			cp := *job
			q.mu.Unlock()
			return &cp, false, nil
		}
		job.Status = StatusCanceled
		cancelFn := q.cancels[id] // only set if currently running
		cp := *job
		q.mu.Unlock()
		if cancelFn != nil {
			cancelFn()
		}
		q.persist(&cp)
		return &cp, true, nil
	}
	q.mu.Unlock()

	if q.store == nil {
		return nil, false, nil
	}
	stored, found, err := q.store.Get(id)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	// Found only in the store, never in q.jobs: Resume() loads every
	// queued/running job into q.jobs at startup, so anything reachable
	// solely through the store is necessarily already terminal.
	return stored, false, nil
}

// WaitForTerminal blocks until the job reaches a terminal state, the
// deadline passes, or ctx is done (e.g. the caller's HTTP connection
// dropped) — whichever comes first. Returns (nil, nil) if the job doesn't
// exist at all, the same not-found-vs-store-error distinction Get() makes.
// Implemented as an internal poll of the desk's own store/memory, not a
// true completion broadcast — simple and correct, and at a 100ms tick the
// caller-visible latency this adds is not worth a channel-based signal for
// the volume this desk is built for.
func (q *Queue) WaitForTerminal(ctx context.Context, id string, deadline time.Time) (*Job, error) {
	const pollInterval = 100 * time.Millisecond
	for {
		job, ok, err := q.Get(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		if job.Status.isTerminal() {
			return job, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return job, nil
		}
		wait := pollInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return job, nil
		case <-time.After(wait):
		}
	}
}

var (
	errJobNotFound    = errors.New("job not found")
	errJobNotTerminal = errors.New("job has not reached a terminal state yet, nothing to ack")
)

// Ack marks a terminal job's result seen/handled by the operator — the
// other half of the inbox model (ListTerminalUnacked is the inbox; Ack
// empties it one item at a time). Acking an already-acked job is a no-op,
// not an error; acking a non-terminal job returns errJobNotTerminal.
func (q *Queue) Ack(id string) (*Job, error) {
	q.mu.Lock()
	job, tracked := q.jobs[id]
	if tracked {
		if !job.Status.isTerminal() {
			q.mu.Unlock()
			return nil, errJobNotTerminal
		}
		if !job.Acked {
			now := time.Now().UTC()
			job.Acked = true
			job.AckedAt = &now
		}
		cp := *job
		q.mu.Unlock()
		q.persist(&cp)
		return &cp, nil
	}
	q.mu.Unlock()

	if q.store == nil {
		return nil, errJobNotFound
	}
	stored, found, err := q.store.Get(id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errJobNotFound
	}
	if !stored.Status.isTerminal() {
		return nil, errJobNotTerminal
	}
	if !stored.Acked {
		now := time.Now().UTC()
		stored.Acked = true
		stored.AckedAt = &now
		if err := q.store.Update(stored); err != nil {
			return nil, err
		}
	}
	return stored, nil
}

// ListTerminalUnacked returns terminal jobs the operator hasn't acked yet —
// the inbox. Store-backed (authoritative, survives restarts) when a store
// is configured; falls back to scanning in-memory jobs under -store=memory.
func (q *Queue) ListTerminalUnacked(limit int) ([]*Job, error) {
	if q.store != nil {
		return q.store.ListTerminalUnacked(limit)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*Job
	for _, j := range q.jobs {
		if j.Status.isTerminal() && !j.Acked {
			cp := *j
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, k int) bool {
		fi, fk := out[i].Finished, out[k].Finished
		if fi == nil || fk == nil {
			return false
		}
		return fi.After(*fk)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListByBatch returns every job submitted as part of the given batch, in
// submission order. Store-backed when configured, same fallback as above.
func (q *Queue) ListByBatch(batchID string) ([]*Job, error) {
	if q.store != nil {
		return q.store.ListByBatch(batchID)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*Job
	for _, j := range q.jobs {
		if j.BatchID == batchID {
			cp := *j
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Created.Before(out[k].Created) })
	return out, nil
}

// WaitForAnyTerminal blocks until at least one terminal-and-unacked job
// exists, the deadline passes, or ctx is done — the zero-latency half of
// the inbox model: park one request here instead of polling
// ListTerminalUnacked yourself in a loop.
func (q *Queue) WaitForAnyTerminal(ctx context.Context, deadline time.Time) ([]*Job, error) {
	const pollInterval = 100 * time.Millisecond
	for {
		jobs, err := q.ListTerminalUnacked(50)
		if err != nil {
			return nil, err
		}
		if len(jobs) > 0 {
			return jobs, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return jobs, nil
		}
		wait := pollInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return jobs, nil
		case <-time.After(wait):
		}
	}
}

// persist is best-effort: a store hiccup must not take down job execution,
// only degrade the desk's ability to resume after a restart.
func (q *Queue) persist(job *Job) {
	if q.store == nil {
		return
	}
	if err := q.store.Update(job); err != nil {
		log.Printf("job %s: persist to store failed: %v", job.ID, err)
	}
}

func (q *Queue) Stats() map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	counts := map[JobStatus]int{}
	var recent []*Job
	for _, j := range q.jobs {
		counts[j.Status]++
		recent = append(recent, j)
	}
	sort.Slice(recent, func(i, k int) bool { return recent[i].Created.After(recent[k].Created) })
	if len(recent) > 10 {
		recent = recent[:10]
	}
	summary := make([]map[string]any, 0, len(recent))
	for _, j := range recent {
		summary = append(summary, map[string]any{
			"id": j.ID, "tool": j.Tool, "status": j.Status, "attempt": j.Attempt,
			"error": j.Error, "created": j.Created,
		})
	}
	return map[string]any{
		"workers": q.workers,
		"queued":  counts[StatusQueued],
		"running": counts[StatusRunning],
		"done":    counts[StatusDone],
		"failed":  counts[StatusFailed],
		"recent":  summary,
	}
}

func newJobID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "job-" + hex.EncodeToString(b)
}

func newBatchID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "batch-" + hex.EncodeToString(b)
}
