package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore is the default JobStore: one file next to the job workspace,
// no server to run, no connection string. Right for the single-desk,
// single-operator deployment this repo is built for; switch to
// PostgresStore (-store=postgres) only if more than one desk instance ever
// needs to share job state over the network.
type SQLiteStore struct {
	db *sql.DB
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id              TEXT PRIMARY KEY,
	tool            TEXT NOT NULL,
	idempotency_key TEXT,
	input           TEXT NOT NULL,
	meta            TEXT,
	status          TEXT NOT NULL,
	attempt         INTEGER NOT NULL DEFAULT 0,
	max_retries     INTEGER NOT NULL DEFAULT 0,
	result          TEXT,
	error           TEXT,
	created_at      TEXT NOT NULL,
	started_at      TEXT,
	finished_at     TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS jobs_tool_idempotency_key_uniq
	ON jobs (tool, idempotency_key) WHERE idempotency_key IS NOT NULL;
`

// sqliteColumnAdditions are new columns layered onto the original schema.
// SQLite has no "ADD COLUMN IF NOT EXISTS", so each is applied via ALTER
// TABLE with the "duplicate column name" error swallowed — an idempotent,
// no-migration-tool migration, same philosophy as the CREATE TABLE IF NOT
// EXISTS above: a fresh DB and an upgraded existing one both end up correct.
var sqliteColumnAdditions = []string{
	"ALTER TABLE jobs ADD COLUMN batch_id TEXT",
	"ALTER TABLE jobs ADD COLUMN acked INTEGER NOT NULL DEFAULT 0",
	"ALTER TABLE jobs ADD COLUMN acked_at TEXT",
}

const sqlitePostSchema = `
CREATE INDEX IF NOT EXISTS jobs_batch_id_idx ON jobs (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS jobs_terminal_unacked_idx ON jobs (acked, status);
`

// timeLayout is used for every timestamp column: SQLite has no native
// datetime type, so times round-trip as RFC3339Nano TEXT instead.
const timeLayout = time.RFC3339Nano

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	// journal_mode(WAL): readers don't block the writer. busy_timeout: a
	// caller that loses a brief write race waits instead of erroring
	// immediately with "database is locked".
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	// SQLite allows exactly one writer at a time. Pinning the pool to a
	// single connection serializes every call through this store instead of
	// letting Go's own connection pool contend with itself for the file
	// lock — busy_timeout above is a safety net, not the primary defense.
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if _, err := db.ExecContext(ctx, sqliteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	for _, ddl := range sqliteColumnAdditions {
		if _, err := db.ExecContext(ctx, ddl); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate: %s: %w", ddl, err)
		}
	}
	if _, err := db.ExecContext(ctx, sqlitePostSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

// Insert writes a new job row. If idempotencyKey is non-empty and a job
// already exists for (tool, idempotencyKey), the existing row is returned
// with created=false and the caller must not enqueue job for execution —
// the desk already has (or had) this exact call in flight.
func (s *SQLiteStore) Insert(job *Job, idempotencyKey string) (existing *Job, created bool, err error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// DO NOTHING (no RETURNING) rather than Postgres's xmax trick: portable
	// across SQLite builds regardless of RETURNING support, and simpler —
	// RowsAffected alone says whether this call won the row.
	const ins = `
INSERT INTO jobs (id, tool, idempotency_key, input, meta, status, attempt, max_retries, created_at, batch_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tool, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
`
	res, err := s.db.ExecContext(ctx, ins,
		job.ID, job.Tool, key, string(inputJSON), string(metaJSON),
		job.Status, job.Attempt, job.MaxRetries, job.Created.UTC().Format(timeLayout),
		nullableString(job.BatchID))
	if err != nil {
		return nil, false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, false, err
	} else if n == 1 {
		return job, true, nil
	}

	// Conflict: another job already owns this (tool, idempotency_key). An
	// empty idempotencyKey never matches the partial unique index, so
	// DO NOTHING never fires for it — this path only runs with a real key.
	row := s.db.QueryRowContext(ctx, sqliteSelectCols+"FROM jobs WHERE tool = ? AND idempotency_key = ?", job.Tool, idempotencyKey)
	got, err := scanSQLiteJob(row)
	if err != nil {
		return nil, false, err
	}
	return got, false, nil
}

// Update persists the current state of an already-inserted job.
func (s *SQLiteStore) Update(job *Job) error {
	resultJSON, err := marshalNullable(job.Result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	const q = `
UPDATE jobs SET status=?, attempt=?, result=?, error=?, started_at=?, finished_at=?
WHERE id=?
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, q,
		job.Status, job.Attempt, nullableJSONText(resultJSON), nullableString(job.Error),
		nullableTime(job.Started), nullableTime(job.Finished), job.ID)
	return err
}

// Ack persists the operator's acknowledgment, and only that — deliberately
// wire-separate from Update so a lifecycle write (a worker finishing a
// job) can never race and clobber this, or vice versa.
func (s *SQLiteStore) Ack(id string, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET acked = 1, acked_at = ? WHERE id = ?",
		at.UTC().Format(timeLayout), id)
	return err
}

// sqliteSelectCols is the fixed column list + order every SELECT below
// uses, so a single scanSQLiteJob can serve all of them without drift.
const sqliteSelectCols = `
SELECT id, tool, idempotency_key, input, meta, status, attempt, max_retries,
       result, error, created_at, started_at, finished_at, batch_id, acked, acked_at
`

func (s *SQLiteStore) Get(id string) (*Job, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, sqliteSelectCols+"FROM jobs WHERE id = ?", id)
	job, err := scanSQLiteJob(row)
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
func (s *SQLiteStore) LoadIncomplete() ([]*Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, sqliteSelectCols+"FROM jobs WHERE status IN ('queued', 'running') ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		job, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListTerminalUnacked returns terminal jobs the operator hasn't acked yet,
// most-recently-finished first — the inbox behind GET /jobs and
// GET /jobs/wait-any.
func (s *SQLiteStore) ListTerminalUnacked(limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const q = sqliteSelectCols + `
FROM jobs WHERE acked = 0 AND status IN ('done', 'failed', 'canceled')
ORDER BY COALESCE(finished_at, created_at) DESC LIMIT ?
`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		job, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListByBatch returns every job submitted under the given batch id, in
// submission order.
func (s *SQLiteStore) ListByBatch(batchID string) ([]*Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, sqliteSelectCols+"FROM jobs WHERE batch_id = ? ORDER BY created_at", batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		job, err := scanSQLiteJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func scanSQLiteJob(row rowScanner) (*Job, error) {
	var (
		job                       Job
		idemKey, batchID          sql.NullString
		inputText, metaText       sql.NullString
		resultText, errText       sql.NullString
		createdText               string
		startedText, finishedText sql.NullString
		acked                     bool
		ackedAtText               sql.NullString
	)
	if err := row.Scan(
		&job.ID, &job.Tool, &idemKey, &inputText, &metaText, &job.Status,
		&job.Attempt, &job.MaxRetries, &resultText, &errText,
		&createdText, &startedText, &finishedText, &batchID, &acked, &ackedAtText,
	); err != nil {
		return nil, err
	}
	job.IdempotencyKey = idemKey.String
	job.Error = errText.String
	job.BatchID = batchID.String
	job.Acked = acked

	created, err := time.Parse(timeLayout, createdText)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	job.Created = created
	if startedText.Valid {
		t, err := time.Parse(timeLayout, startedText.String)
		if err != nil {
			return nil, fmt.Errorf("parse started_at: %w", err)
		}
		job.Started = &t
	}
	if finishedText.Valid {
		t, err := time.Parse(timeLayout, finishedText.String)
		if err != nil {
			return nil, fmt.Errorf("parse finished_at: %w", err)
		}
		job.Finished = &t
	}
	if ackedAtText.Valid {
		t, err := time.Parse(timeLayout, ackedAtText.String)
		if err != nil {
			return nil, fmt.Errorf("parse acked_at: %w", err)
		}
		job.AckedAt = &t
	}
	if inputText.Valid && inputText.String != "" {
		if err := json.Unmarshal([]byte(inputText.String), &job.Input); err != nil {
			return nil, fmt.Errorf("unmarshal input: %w", err)
		}
	}
	if metaText.Valid && metaText.String != "" {
		if err := json.Unmarshal([]byte(metaText.String), &job.Meta); err != nil {
			return nil, fmt.Errorf("unmarshal meta: %w", err)
		}
	}
	if resultText.Valid && resultText.String != "" {
		if err := json.Unmarshal([]byte(resultText.String), &job.Result); err != nil {
			return nil, fmt.Errorf("unmarshal result: %w", err)
		}
	}
	return &job, nil
}

func nullableJSONText(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeLayout)
}
