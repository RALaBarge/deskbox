package main

import "encoding/json"

// JobStore is the durable job backend the desk enforces idempotency and
// crash-resume through. Postgres and SQLite both ship in this repo, but
// neither is special: implement this interface with whatever you actually
// run (Redis, MySQL, a flat file, a wire call to something else entirely),
// pass it to NewQueue, and the desk doesn't care which one it's talking to.
type JobStore interface {
	// Insert writes a new job row. If idempotencyKey is non-empty and a job
	// already exists for (tool, idempotencyKey), the existing row is
	// returned with created=false and the caller must not enqueue the job
	// for execution — the desk already has (or had) this exact call in
	// flight.
	Insert(job *Job, idempotencyKey string) (existing *Job, created bool, err error)
	// Update persists the current state of an already-inserted job.
	Update(job *Job) error
	// Get looks up one job by id.
	Get(id string) (*Job, bool, error)
	// LoadIncomplete returns every job left "queued" or "running" from a
	// prior process lifetime, so a desk restart resumes work instead of
	// silently dropping or duplicating it.
	LoadIncomplete() ([]*Job, error)
	Close() error
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows — shared by every
// JobStore implementation's row-scanning helper.
type rowScanner interface {
	Scan(dest ...any) error
}

func marshalNullable(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
