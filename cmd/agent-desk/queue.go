package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

type JobStatus string

const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusDone    JobStatus = "done"
	StatusFailed  JobStatus = "failed"
)

// Job is one tool invocation flowing through the desk. Job IDs are what the
// agent polls; the desk keeps the history in memory so the contract story is
// auditable ("what did the agent try, and why did it not conform?").
type Job struct {
	ID         string         `json:"id"`
	Tool       string         `json:"tool"`
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
}

type Queue struct {
	mu      sync.Mutex
	desk    *Desk
	jobs    map[string]*Job
	ch      chan *Job
	workers int
	stop    chan struct{}
	wg      sync.WaitGroup
}

func NewQueue(workers int) *Queue {
	if workers < 1 {
		workers = 1
	}
	return &Queue{
		jobs:    map[string]*Job{},
		ch:      make(chan *Job, 256),
		workers: workers,
		stop:    make(chan struct{}),
	}
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

func (q *Queue) Submit(tool *Tool, input, meta map[string]any) (*Job, error) {
	job := &Job{
		ID: newJobID(), Tool: tool.Name, Input: input, Meta: meta,
		Status: StatusQueued, MaxRetries: tool.Execution.MaxRetries,
		Created: time.Now().UTC(),
	}
	q.mu.Lock()
	q.jobs[job.ID] = job
	q.mu.Unlock()
	select {
	case q.ch <- job:
		return job, nil
	case <-q.stop:
		return nil, errors.New("queue stopped")
	}
}

func (q *Queue) Get(id string) (*Job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	return j, ok
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
	now := time.Now().UTC()
	q.mu.Lock()
	job.Status = StatusRunning
	job.Attempt++
	job.Started = &now
	q.mu.Unlock()

	result, err := q.desk.Execute(tool, job, job.Input)

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
		time.Sleep(backoff)
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
	defer q.mu.Unlock()
	job.Finished = &now
	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
		return
	}
	job.Status = StatusDone
	job.Result = result
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