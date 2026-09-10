package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const version = "0.1.0"

// maxWait caps how long any single HTTP request (POST /tools/{name}?wait=,
// GET /jobs/{id}/wait?timeout=) will block, so a caller can't hold a
// connection open indefinitely by accident — and so the desk stays well
// under typical reverse-proxy/load-balancer timeouts. A wait longer than
// this needs actual polling (GET /jobs/{id}, or /wait again).
const maxWait = 55 * time.Second

// Desk is the enforcing proxy. Every tool invocation an agent makes must pass
// through here, so the contract (TCS) is load-bearing: inputs are validated on
// the way in, side effects and output schema on the way out.
type Desk struct {
	tools    map[string]*Tool
	queue    *Queue
	dataDir  string
	settings *Settings
	// resourceLimitsOK: systemd-run --user --scope confirmed working at
	// startup. Can flip false at runtime if a job discovers it stopped
	// working (see the "Failed to connect to bus" handling in Execute) —
	// atomic because concurrent workers read and (rarely) write it.
	resourceLimitsOK atomic.Bool
}

func NewDesk(tools map[string]*Tool, q *Queue, dataDir string, settings *Settings, resourceLimitsOK bool) *Desk {
	d := &Desk{tools: tools, queue: q, dataDir: dataDir, settings: settings}
	d.resourceLimitsOK.Store(resourceLimitsOK)
	q.desk = d
	return d
}

func main() {
	if err := loadDotenv(".env"); err != nil {
		log.Fatalf("load .env: %v", err)
	}
	settings, err := LoadSettings()
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	// deskbox.yaml: structural desk config a human edits and commits (unlike
	// .env, which is gitignored and secret-only). A missing file changes
	// nothing — every field below still has a working default.
	cfg, err := LoadConfig("deskbox.yaml")
	if err != nil {
		log.Fatalf("deskbox.yaml: %v", err)
	}

	// Precedence for store selection, highest wins: explicit -store flag >
	// DESKBOX_STORE env > deskbox.yaml's store.kind > hardcoded "sqlite".
	// Each layer only overrides the next if it actually set something.
	storeKindDefault := settings.StoreKind
	if storeKindDefault == "" {
		storeKindDefault = cfg.Store.Kind
	}
	if storeKindDefault == "" {
		storeKindDefault = "sqlite"
	}
	sqlitePathDefault := settings.SQLitePath
	if sqlitePathDefault == "" {
		sqlitePathDefault = cfg.Store.SQLite.Path
	}

	addr := flag.String("addr", ":8080", "listen address (host:port)")
	toolsDir := flag.String("tools", "tools", "directory of tool folders; each folder must contain tcs.yaml")
	workerCount := flag.Int("workers", 10, "number of queued-execution workers")
	dataDir := flag.String("data", defaultDataDir(), "job workspace root (jobs/<id>/in, jobs/<id>/out live here, tail-able)")
	storeKind := flag.String("store", storeKindDefault, "durable job backend: sqlite (default) | postgres | memory")
	sqlitePath := flag.String("sqlite-path", sqlitePathDefault,
		"SQLite file for durable jobs + idempotency_key dedup (default: <data>/deskbox.db)")
	postgresDSN := flag.String("postgres-dsn", settings.PostgresDSN,
		"Postgres DSN for durable jobs + idempotency_key dedup (only used with -store=postgres; secret, keep it in .env, not deskbox.yaml)")
	flag.Parse()

	if settings.AuthEnabled && settings.AuthToken == "" {
		log.Fatalf("DESKBOX_AUTH_ENABLED is set but DESKBOX_AUTH_TOKEN is empty")
	}
	if settings.AuthEnabled {
		log.Printf("auth: enabled — requests need Authorization: Bearer <token>")
	} else {
		log.Printf("auth: disabled — anything that can reach %s can submit jobs", *addr)
	}

	tools, err := LoadTools(*toolsDir)
	if err != nil {
		log.Fatalf("load tools: %v", err)
	}
	log.Printf("deskbox agent-desk %s: loaded %d tool(s) from %s", version, len(tools), *toolsDir)

	if hasBwrap() {
		log.Printf("sandbox: bubblewrap available — network isolation active")
	} else {
		log.Printf("sandbox: WARNING bubblewrap NOT found — network:false enforced as advisory only")
	}

	resourceLimitsOK := canScopeJobs()
	if resourceLimitsOK {
		log.Printf("resource limits: systemd-run --user --scope available — jobs capped at %s memory, %d tasks",
			settings.JobMemoryMax, settings.JobTasksMax)
	} else {
		log.Printf("WARN: systemd-run --user --scope not usable here (no user D-Bus session? see README) — " +
			"job memory/task-count limits are NOT enforced")
	}

	// JobStore is an interface (store.go): sqlite and postgres both ship
	// here, but neither is privileged — any type satisfying JobStore can be
	// passed to NewQueue instead.
	var store JobStore
	switch strings.ToLower(*storeKind) {
	case "memory":
		log.Printf("store: memory only — no idempotency or crash-resume across restarts")
	case "postgres":
		if *postgresDSN == "" {
			log.Fatalf("-store=postgres requires -postgres-dsn (or DESKBOX_POSTGRES_DSN)")
		}
		s, err := NewPostgresStore(*postgresDSN)
		if err != nil {
			log.Fatalf("postgres store: %v", err)
		}
		defer s.Close()
		store = s
		log.Printf("store: postgres — jobs are durable, idempotency_key is enforced")
	case "sqlite", "":
		path := *sqlitePath
		if path == "" {
			path = filepath.Join(*dataDir, "deskbox.db")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			log.Fatalf("sqlite store: create dir for %s: %v", path, err)
		}
		s, err := NewSQLiteStore(path)
		if err != nil {
			log.Fatalf("sqlite store: %v", err)
		}
		defer s.Close()
		store = s
		log.Printf("store: sqlite (%s) — jobs are durable, idempotency_key is enforced", path)
	default:
		log.Fatalf("-store=%q not recognized (sqlite | postgres | memory)", *storeKind)
	}

	q := NewQueue(*workerCount, store)
	q.Start()
	defer q.Stop()

	d := NewDesk(tools, q, *dataDir, settings, resourceLimitsOK)

	if n, err := q.Resume(); err != nil {
		log.Printf("resume from store: %v", err)
	} else if n > 0 {
		log.Printf("resume from store: re-enqueued %d incomplete job(s)", n)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", d.handleStatus)
	mux.HandleFunc("GET /tools", d.handleListTools)
	mux.HandleFunc("GET /tools/{name}", d.handleGetTool)
	mux.HandleFunc("POST /tools/{name}", d.handleSubmit)
	mux.HandleFunc("GET /jobs", d.handleListInbox)
	mux.HandleFunc("GET /jobs/wait-any", d.handleWaitAnyJob)
	mux.HandleFunc("GET /jobs/{id}", d.handleGetJob)
	mux.HandleFunc("GET /jobs/{id}/wait", d.handleWaitJob)
	mux.HandleFunc("POST /jobs/{id}/ack", d.handleAckJob)
	mux.HandleFunc("DELETE /jobs/{id}", d.handleCancelJob)
	mux.HandleFunc("GET /jobs/{id}/out/{file...}", d.handleGetOutFile)
	mux.HandleFunc("POST /batches", d.handleCreateBatch)
	mux.HandleFunc("GET /batches/{id}", d.handleGetBatch)
	mux.HandleFunc("GET /queue", d.handleQueueStats)

	var handler http.Handler = mux
	if settings.AuthEnabled {
		handler = requireAuth(settings.AuthToken, mux)
	}

	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("agent-desk listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

// requireAuth rejects any request without a matching Authorization: Bearer
// <token> header. Comparison is constant-time so response timing can't leak
// how much of the token a guess got right.
func requireAuth(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d *Desk) handleStatus(w http.ResponseWriter, r *http.Request) {
	s := d.queue.Stats()
	s["service"] = "deskbox-agent-desk"
	s["version"] = version
	s["tools"] = len(d.tools)
	writeJSON(w, http.StatusOK, s)
}

func (d *Desk) handleListTools(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(d.tools))
	for n := range d.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		t := d.tools[n]
		out = append(out, map[string]any{
			"name":                 t.Name,
			"summary":              t.Summary,
			"execution_mode":       orDefault(t.Execution.Mode, "queued"),
			"max_retries":          t.Execution.MaxRetries,
			"timeout_ms":           t.Execution.TimeoutMS,
			"allowed_side_effects": t.AllowedSideEffects,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGetTool returns the TCS contract an agent must conform to before it
// is allowed to invoke this tool. tcs.yaml is the authoring format on disk;
// every response this API sends is JSON with no exceptions, so it's
// re-decoded generically (not through the Tool struct, so no field the
// struct doesn't model is silently dropped) rather than served as raw YAML.
func (d *Desk) handleGetTool(w http.ResponseWriter, r *http.Request) {
	t, ok := d.tools[r.PathValue("name")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tool not found"})
		return
	}
	contract, err := t.AsJSON()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "contract failed to decode: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, contract)
}

type submitRequest struct {
	Input map[string]any `json:"input"`
	Meta  map[string]any `json:"meta,omitempty"`
	// IdempotencyKey, when set, makes a repeated submit for this tool a no-op:
	// the desk returns the existing job instead of running the tool again.
	// Ignored (never deduped) with -store=memory.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// parseWait reads ?wait=<duration> (e.g. "5s", "500ms") for the unified
// invoke path. Missing or empty is 0 — the original async-first behavior
// for a queued tool: 202 + Location, no blocking. A positive value blocks
// up to that long for the job to finish before falling back to 202,
// clamped to maxWait so a caller can't hold the connection open forever by
// accident. A "direct" tool ignores this entirely (see handleSubmit) — it
// always waits for its own result, that's what declaring it direct means.
func parseWait(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("wait")
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid wait duration %q: %w", raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("wait must not be negative")
	}
	if d > maxWait {
		d = maxWait
	}
	return d, nil
}

// handleSubmit is the only way to run a tool. Every call is persisted and
// queued the same way regardless of the tool's execution.mode — that
// distinction now only controls how long this handler waits before
// responding, not whether the job goes through the store, retries, or
// idempotency dedup. (Previously "direct" ran inline in this handler,
// bypassing the worker pool, retries, and the store entirely — a real
// inconsistency: a direct tool's own max_retries was silently ignored, and
// it wasn't governed by -workers like every other tool. Folding it into
// the same path fixes that, at the cost of a direct call now possibly
// waiting on a free worker slot under heavy load, same as a queued one.)
func (d *Desk) handleSubmit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tool, ok := d.tools[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tool not found: " + name})
		return
	}

	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Input == nil {
		req.Input = map[string]any{}
	}

	// The gate: input must conform to the tool's contract before anything runs.
	if tool.Input != nil {
		if viol := Validate(*tool.Input, req.Input); len(viol) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":      "contract violation: input does not conform to the tool contract",
				"violations": viol,
			})
			return
		}
	}

	wait, err := parseWait(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	job, err := d.queue.Submit(tool, req.Input, req.Meta, req.IdempotencyKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if !tool.Execution.IsQueued() {
		// direct always waits for the final result, bounded by the tool's
		// own timeout/retry budget as a safety cap — not caller-tunable.
		wait = tool.Execution.MaxTotalDuration()
	}
	if wait <= 0 {
		w.Header().Set("Location", "/jobs/"+job.ID)
		writeJSON(w, http.StatusAccepted, job)
		return
	}

	final, err := d.queue.WaitForTerminal(r.Context(), job.ID, time.Now().Add(wait))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	w.Header().Set("Location", "/jobs/"+final.ID)
	if final.Status.isTerminal() {
		writeJSON(w, http.StatusOK, final)
		return
	}
	writeJSON(w, http.StatusAccepted, final) // still in flight when the wait elapsed
}

// handleWaitJob long-polls: blocks until the job reaches a terminal state
// or ?timeout elapses (default 25s, capped at maxWait), instead of the
// caller hammering GET /jobs/{id} in a tight loop.
func (d *Desk) handleWaitJob(w http.ResponseWriter, r *http.Request) {
	timeout := 25 * time.Second
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid timeout duration %q: %v", raw, err)})
			return
		}
		if parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "timeout must not be negative"})
			return
		}
		if parsed > maxWait {
			parsed = maxWait
		}
		timeout = parsed
	}

	job, err := d.queue.WaitForTerminal(r.Context(), r.PathValue("id"), time.Now().Add(timeout))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	if job.Status.isTerminal() {
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeJSON(w, http.StatusAccepted, job) // still in flight when timeout elapsed
}

// handleCancelJob cancels a queued or running job. A queued job is skipped
// the moment its worker turn comes; a running job's process is killed via
// its context (see Queue.Cancel / Queue.process). A job that already
// reached a terminal state is a 409, not an error — cancellation is a
// no-op there, not a failure.
func (d *Desk) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	job, canceled, err := d.queue.Cancel(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	if !canceled {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "job already " + string(job.Status) + ", nothing to cancel",
			"job":   job,
		})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleListInbox is the inbox: every terminal job the operator hasn't
// acked yet, most-recently-finished first. This is the desk's replacement
// for a claim/TTL delivery mechanism — with one always-on desk process
// instead of many independent ones, there's no "who's currently watching"
// race to arbitrate: multiple readers seeing the same unacked job is
// harmless, so this is a plain query, not a claim. Nothing is lost if
// nobody polls it for a while — unacked rows just sit there.
func (d *Desk) handleListInbox(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be a positive integer"})
			return
		}
		limit = n
	}
	jobs, err := d.queue.ListTerminalUnacked(limit)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleWaitAnyJob long-polls the inbox: blocks until at least one job is
// terminal-and-unacked, or ?timeout elapses (default 25s, capped at
// maxWait) — instead of polling GET /jobs yourself in a loop. The
// zero-latency half of the inbox model.
func (d *Desk) handleWaitAnyJob(w http.ResponseWriter, r *http.Request) {
	timeout := 25 * time.Second
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid timeout duration %q: %v", raw, err)})
			return
		}
		if parsed < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "timeout must not be negative"})
			return
		}
		if parsed > maxWait {
			parsed = maxWait
		}
		timeout = parsed
	}
	jobs, err := d.queue.WaitForAnyTerminal(r.Context(), time.Now().Add(timeout))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if jobs == nil {
		jobs = []*Job{} // timed out with nothing unacked: {"jobs":[]}, not null
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleAckJob marks a terminal job's result seen/handled — the other half
// of the inbox model (handleListInbox is the inbox; this empties it one
// item at a time). Acking a non-terminal job is a 409: there's nothing to
// ack yet. Acking an already-acked job is idempotent, not an error.
func (d *Desk) handleAckJob(w http.ResponseWriter, r *http.Request) {
	job, err := d.queue.Ack(r.PathValue("id"))
	switch {
	case errors.Is(err, errJobNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
	case errors.Is(err, errJobNotTerminal):
		writeJSON(w, http.StatusConflict, map[string]any{"error": errJobNotTerminal.Error()})
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
	default:
		writeJSON(w, http.StatusOK, job)
	}
}

type batchRequest struct {
	Tool  string           `json:"tool"`
	Items []map[string]any `json:"items"`
	Meta  map[string]any   `json:"meta,omitempty"`
}

// handleCreateBatch is the first-class replacement for lease-based chunk
// claiming: one job per item, fanned out under a shared batch id, each
// going through the exact same queue/store/retry path as any other job.
// A dead item is just a failed job on the existing retry path; a desk
// restart mid-batch resumes via Resume/LoadIncomplete same as any other
// in-flight job — there is one worker (the desk itself), so there's
// nothing to lease from itself if it goes away and comes back.
func (d *Desk) handleCreateBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body: " + err.Error()})
		return
	}
	tool, ok := d.tools[req.Tool]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tool not found: " + req.Tool})
		return
	}
	if len(req.Items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "items must be a non-empty array"})
		return
	}

	items := make([]map[string]any, len(req.Items))
	for i, item := range req.Items {
		if item == nil {
			item = map[string]any{}
		}
		items[i] = item
	}

	// The gate, same as a single submit — but for every item, before any
	// job is created: an invalid item anywhere fails the whole batch, not
	// a partial fan-out with some items silently skipped.
	if tool.Input != nil {
		violations := map[string]any{}
		for i, item := range items {
			if viol := Validate(*tool.Input, item); len(viol) > 0 {
				violations[strconv.Itoa(i)] = viol
			}
		}
		if len(violations) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":      "contract violation: one or more items do not conform to the tool contract",
				"violations": violations,
			})
			return
		}
	}

	batchID, jobs, err := d.queue.SubmitBatch(tool, items, req.Meta)
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.ID
	}
	if err != nil {
		// Every item already validated above, so this is a real store
		// failure partway through the fan-out — items before it are
		// already persisted and queued, not lost, and must not be. Return
		// their ids rather than discarding them: without batch_id/job_ids
		// here, a caller has no way to find and reconcile jobs that are
		// already running under a batch it was never told about, and its
		// only visible move (resubmit) would duplicate them (batch items
		// carry no idempotency key of their own).
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":         err.Error(),
			"batch_id":      batchID,
			"job_ids":       jobIDs,
			"items_pending": len(items) - len(jobs),
		})
		return
	}
	w.Header().Set("Location", "/batches/"+batchID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"batch_id": batchID,
		"tool":     tool.Name,
		"job_ids":  jobIDs,
	})
}

// handleGetBatch reports batch progress: items done/total, broken down by
// status. This is the "content-progress heartbeat" flagged as unbuilt in
// the old system's own design notes — here for free, as a side effect of
// a batch item just being an ordinary job the desk already tracks.
func (d *Desk) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	batchID := r.PathValue("id")
	jobs, err := d.queue.ListByBatch(batchID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if len(jobs) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "batch not found"})
		return
	}
	counts := map[JobStatus]int{}
	for _, j := range jobs {
		counts[j.Status]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batch_id": batchID,
		"total":    len(jobs),
		"queued":   counts[StatusQueued],
		"running":  counts[StatusRunning],
		"done":     counts[StatusDone],
		"failed":   counts[StatusFailed],
		"canceled": counts[StatusCanceled],
		"jobs":     jobs,
	})
}

func (d *Desk) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok, err := d.queue.Get(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleGetOutFile streams a file from the job's out/ dir — the "simply tail"
// surface. Works while the job is still running, and stays available after.
//
// Only names the tool's own contract declares in sandbox.out are servable,
// and only if the path is a regular file: the desk bind-mounts out/
// read-write into the sandbox, so a tool could otherwise write a symlink
// (e.g. to /etc/passwd or the host's .env) and have the desk read the
// symlink's target on its behalf once the sandbox is gone.
func (d *Desk) handleGetOutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok, err := d.queue.Get(id)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "job store temporarily unavailable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	rel := r.PathValue("file")
	if pathTraverses(rel) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
		return
	}
	tool, ok := d.tools[job.Tool]
	if !ok || !declaresOut(tool, rel) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not a declared output file for this tool"})
		return
	}
	base := filepath.Join(d.dataDir, "jobs", id, "out")
	p := filepath.Join(base, filepath.Clean(rel))
	if !strings.HasPrefix(p, base+string(filepath.Separator)) && p != base {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
		return
	}
	fi, statErr := os.Lstat(p)
	if statErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "output file not yet written"})
		return
	}
	if !fi.Mode().IsRegular() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "refusing to serve a non-regular file"})
		return
	}
	http.ServeFile(w, r, p)
}

func declaresOut(tool *Tool, rel string) bool {
	for _, o := range tool.Sandbox.Out {
		if filepath.Clean(o) == filepath.Clean(rel) {
			return true
		}
	}
	return false
}

func (d *Desk) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.queue.Stats())
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "deskbox-data")
	}
	return filepath.Join(home, ".local", "share", "deskbox")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
