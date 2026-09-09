package main

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const version = "0.1.0"

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

	addr := flag.String("addr", ":8080", "listen address (host:port)")
	toolsDir := flag.String("tools", "tools", "directory of tool folders; each folder must contain tcs.yaml")
	workerCount := flag.Int("workers", 10, "number of queued-execution workers")
	dataDir := flag.String("data", defaultDataDir(), "job workspace root (jobs/<id>/in, jobs/<id>/out live here, tail-able)")
	postgresDSN := flag.String("postgres-dsn", settings.PostgresDSN,
		"Postgres DSN for durable jobs + idempotency_key dedup (optional; unset = in-memory only)")
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

	var store *Store
	if *postgresDSN != "" {
		s, err := NewStore(*postgresDSN)
		if err != nil {
			log.Fatalf("postgres: %v", err)
		}
		defer s.Close()
		store = s
		log.Printf("postgres: connected — jobs are durable, idempotency_key is enforced")
	} else {
		log.Printf("postgres: not configured — jobs are in-memory only, no idempotency across restarts")
	}

	q := NewQueue(*workerCount, store)
	q.Start()
	defer q.Stop()

	d := NewDesk(tools, q, *dataDir, settings, resourceLimitsOK)

	if n, err := q.Resume(); err != nil {
		log.Printf("resume from postgres: %v", err)
	} else if n > 0 {
		log.Printf("resume from postgres: re-enqueued %d incomplete job(s)", n)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", d.handleStatus)
	mux.HandleFunc("GET /tools", d.handleListTools)
	mux.HandleFunc("GET /tools/{name}", d.handleGetTool)
	mux.HandleFunc("POST /tools/{name}", d.handleSubmit)
	mux.HandleFunc("GET /jobs/{id}", d.handleGetJob)
	mux.HandleFunc("GET /jobs/{id}/out/{file...}", d.handleGetOutFile)
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
	// Requires -postgres-dsn; ignored (never deduped) without it.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

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

	if tool.Execution.IsQueued() {
		job, err := d.queue.Submit(tool, req.Input, req.Meta, req.IdempotencyKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Location", "/jobs/"+job.ID)
		writeJSON(w, http.StatusAccepted, job)
		return
	}

	// mode: direct — run synchronously, no queue.
	now := time.Now().UTC()
	job := &Job{ID: newJobID(), Tool: tool.Name, Input: req.Input, Meta: req.Meta,
		Status: StatusRunning, Attempt: 1, MaxRetries: tool.Execution.MaxRetries,
		Created: now, Started: &now}
	result, err := d.Execute(tool, job, req.Input)
	if err != nil {
		job.Status = StatusFailed
		job.Error = err.Error()
	} else {
		job.Status = StatusDone
		job.Result = result
	}
	fin := time.Now().UTC()
	job.Finished = &fin
	writeJSON(w, http.StatusOK, job)
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
