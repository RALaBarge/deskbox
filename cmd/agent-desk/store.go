package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Store is the durable job record in Postgres. It backs the in-memory Queue
// with two things the queue alone can't provide: survival across a desk
// restart, and idempotency — if an agent (re)submits the same tool call with
// the same idempotency_key, it gets back the one job that already exists
// instead of triggering a second execution.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	tool            TEXT NOT NULL,
	idempotency_key TEXT,
	input           JSONB NOT NULL,
	meta            JSONB,
	status          TEXT NOT NULL,
	attempt         INT NOT NULL DEFAULT 0,
	max_retries     INT NOT NULL DEFAULT 0,
	result          JSONB,
	error           TEXT,
	created_at      TIMESTAMPTZ NOT NULL,
	started_at      TIMESTAMPTZ,
	finished_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_tool_idempotency_key_uniq
	ON jobs (tool, idempotency_key) WHERE idempotency_key IS NOT NULL;
`

func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Insert writes a new job row. If idempotencyKey is non-empty and a job
// already exists for (tool, idempotencyKey), the existing row is returned
// with created=false and the caller must not enqueue job for execution —
// the desk already has (or had) this exact call in flight.
func (s *Store) Insert(job *Job, idempotencyKey string) (existing *Job, created bool, err error) {
	inputJSON, err := json.Marshal(job.Input)
	if err != nil {
		return nil, false, fmt.Errorf("marshal input: %w", err)
	}
	metaJSON, err := json.Marshal(job.Meta)
	if err != nil {
		return nil, false, fmt.Errorf("marshal meta: %w", err)
	}

	var key any
	if idempotencyKey != "" {
		key = idempotencyKey
	}

	const q = `
INSERT INTO jobs (id, tool, idempotency_key, input, meta, status, attempt, max_retries, created_at)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9)
ON CONFLICT (tool, idempotency_key) WHERE idempotency_key IS NOT NULL
DO UPDATE SET tool = jobs.tool
RETURNING id, tool, idempotency_key, input, meta, status, attempt, max_retries,
          result, error, created_at, started_at, finished_at, (xmax = 0) AS inserted
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, q,
		job.ID, job.Tool, key, inputJSON, metaJSON,
		job.Status, job.Attempt, job.MaxRetries, job.Created)

	var inserted bool
	got, err := scanJob(row, &inserted)
	if err != nil {
		return nil, false, err
	}
	return got, inserted, nil
}

// Update persists the current state of an already-inserted job.
func (s *Store) Update(job *Job) error {
	resultJSON, err := marshalNullable(job.Result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	const q = `
UPDATE jobs SET status=$2, attempt=$3, result=$4::jsonb, error=$5, started_at=$6, finished_at=$7
WHERE id=$1
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, q,
		job.ID, job.Status, job.Attempt, resultJSON, nullableString(job.Error),
		job.Started, job.Finished)
	return err
}

func (s *Store) Get(id string) (*Job, bool, error) {
	const q = `
SELECT id, tool, idempotency_key, input, meta, status, attempt, max_retries,
       result, error, created_at, started_at, finished_at
FROM jobs WHERE id = $1
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, q, id)
	job, err := scanJob(row, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return job, true, nil
}

// LoadIncomplete returns every job left "queued" or "running" from a prior
// process lifetime — the set that must be resumed on startup so a desk
// restart doesn't silently drop or duplicate work already accepted.
func (s *Store) LoadIncomplete() ([]*Job, error) {
	const q = `
SELECT id, tool, idempotency_key, input, meta, status, attempt, max_retries,
       result, error, created_at, started_at, finished_at
FROM jobs WHERE status IN ('queued', 'running') ORDER BY created_at
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows, nil)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner, inserted *bool) (*Job, error) {
	var (
		job                 Job
		idemKey             sql.NullString
		inputJSON, metaJSON []byte
		resultJSON          []byte
		errText             sql.NullString
		started, finished   sql.NullTime
	)
	dest := []any{
		&job.ID, &job.Tool, &idemKey, &inputJSON, &metaJSON, &job.Status,
		&job.Attempt, &job.MaxRetries, &resultJSON, &errText,
		&job.Created, &started, &finished,
	}
	if inserted != nil {
		dest = append(dest, inserted)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	job.IdempotencyKey = idemKey.String
	job.Error = errText.String
	if started.Valid {
		job.Started = &started.Time
	}
	if finished.Valid {
		job.Finished = &finished.Time
	}
	if len(inputJSON) > 0 {
		if err := json.Unmarshal(inputJSON, &job.Input); err != nil {
			return nil, fmt.Errorf("unmarshal input: %w", err)
		}
	}
	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &job.Meta); err != nil {
			return nil, fmt.Errorf("unmarshal meta: %w", err)
		}
	}
	if len(resultJSON) > 0 {
		if err := json.Unmarshal(resultJSON, &job.Result); err != nil {
			return nil, fmt.Errorf("unmarshal result: %w", err)
		}
	}
	return &job, nil
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
