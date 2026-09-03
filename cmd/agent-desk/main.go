package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const version = "0.1.0"

// Desk is the enforcing proxy. Every tool invocation an agent makes must pass
// through here, so the contract (TCS) is load-bearing: inputs are validated on
// the way in, side effects and output schema on the way out.
type Desk struct {
	tools   map[string]*Tool
	queue   *Queue
	dataDir string
}

func NewDesk(tools map[string]*Tool, q *Queue, dataDir string) *Desk {
	d := &Desk{tools: tools, queue: q, dataDir: dataDir}
	q.desk = d
	return d
}

func main() {
	addr := flag.String("addr", ":8080", "listen address (host:port)")
	toolsDir := flag.String("tools", "tools", "directory of tool folders; each folder must contain tcs.yaml")
	workerCount := flag.Int("workers", 2, "number of queued-execution workers")
	dataDir := flag.String("data", defaultDataDir(), "job workspace root (jobs/<id>/in, jobs/<id>/out live here, tail-able)")
	postgresDSN := flag.String("postgres-dsn", os.Getenv("DESKBOX_POSTGRES_DSN"),
		"Postgres DSN for durable jobs + idempotency_key dedup (optional; unset = in-memory only)")
	flag.Parse()

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

	d := NewDesk(tools, q, *dataDir)

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

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("agent-desk listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
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

// handleGetTool returns the raw TCS yaml — the contract an agent must conform
// to before it is allowed to invoke this tool.
func (d *Desk) handleGetTool(w http.ResponseWriter, r *http.Request) {
	t, ok := d.tools[r.PathValue("name")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tool not found"})
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(t.raw)
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
	job, ok := d.queue.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleGetOutFile streams a file from the job's out/ dir — the "simply tail"
// surface. Works while the job is still running, and stays available after.
func (d *Desk) handleGetOutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := d.queue.Get(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	rel := r.PathValue("file")
	if pathTraverses(rel) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
		return
	}
	base := filepath.Join(d.dataDir, "jobs", id, "out")
	p := filepath.Join(base, filepath.Clean(rel))
	if !strings.HasPrefix(p, base+string(filepath.Separator)) && p != base {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
		return
	}
	if _, err := os.Stat(p); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "output file not yet written"})
		return
	}
	http.ServeFile(w, r, p)
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
