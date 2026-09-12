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

// version is overridable at build time so a shipped binary can say exactly
// what it is:
//
//	go build -ldflags "-X main.version=v0.2.0 -X main.commit=$(git rev-parse --short HEAD)"
var (
	version = "0.1.0"
	commit  = ""
)

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func versionString() string {
	if commit == "" {
		return "deskbox agent-desk " + version
	}
	return "deskbox agent-desk " + version + " (" + commit + ")"
}

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
	// sandboxOK: bubblewrap was present at startup. Fixed for the process
	// lifetime — unlike resource limits, bwrap doesn't stop existing.
	sandboxOK bool
	// resourceLimitsOK: systemd-run --user --scope confirmed working at
	// startup. Can flip false at runtime if a job discovers it stopped
	// working (see the "Failed to connect to bus" handling in Execute) —
	// atomic because concurrent workers read and (rarely) write it.
	resourceLimitsOK atomic.Bool
}

func NewDesk(tools map[string]*Tool, q *Queue, dataDir string, settings *Settings, sandboxOK, resourceLimitsOK bool) *Desk {
	d := &Desk{tools: tools, queue: q, dataDir: dataDir, settings: settings, sandboxOK: sandboxOK}
	d.resourceLimitsOK.Store(resourceLimitsOK)
	q.desk = d
	return d
}

// enforcement reports whether each guarantee the desk advertises is
// actually in force right now, so an operator can check without reading
// the startup log — and so a monitoring probe can watch one boolean.
//
// "degraded" covers only mechanisms the desk tried to use and couldn't:
// the sandbox and the per-job caps. Auth being off is a deployment choice
// rather than a failure, so it lands in warnings without setting degraded.
func (d *Desk) enforcement() map[string]any {
	sandbox := d.sandboxOK
	limits := d.resourceLimitsOK.Load()

	warnings := []string{}
	if !sandbox {
		warnings = append(warnings, "bubblewrap not found: tools run unsandboxed, and network:false "+
			"is advisory only rather than enforced by the kernel")
	}
	if !limits {
		warnings = append(warnings, "systemd-run --user --scope not usable: per-job memory and "+
			"task-count limits are not enforced")
	}
	// "Auth is off" means something different depending on what the desk is
	// listening on. On a 0600 unix socket the kernel has already answered
	// the access question — only one account can open it — so reporting an
	// open door there is just noise that trains an operator to ignore the
	// list. On a TCP port it is the real warning, and how far it reaches
	// decides how loud.
	socket := isUnixSocket(d.settings.ListenAddr)
	if !d.settings.AuthEnabled && !socket {
		if listensBeyondLoopback(d.settings.ListenAddr) {
			warnings = append(warnings, "auth disabled on a routable address ("+d.settings.ListenAddr+
				"): anything that can route to this box can submit jobs")
		} else {
			warnings = append(warnings, "auth disabled: any local process, as any user on this box, "+
				"can submit jobs that then run as "+describeUser()+
				" — loopback stops the network, not other accounts")
		}
	}
	root := runningAsRoot()
	if root {
		warnings = append(warnings, "running as root: tools run as uid 0 inside their sandbox, "+
			"which is the one thing that makes the sandbox worth much less than it looks")
	}

	return map[string]any{
		"strict":          d.settings.Strict,
		"sandbox":         sandbox,
		"resource_limits": limits,
		"auth":            d.settings.AuthEnabled,
		"listen":          d.settings.ListenAddr,
		// A 0600 socket is access control the kernel enforces on connect,
		// which is a stronger statement than a shared secret in a header.
		"user_scoped": socket,
		// The account tools run as is the ceiling on what any of them can
		// do — reported alongside the mechanisms because it bounds them.
		"user":     describeUser(),
		"root":     root,
		"degraded": !sandbox || !limits || root,
		"warnings": warnings,
	}
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

	// Loopback by default, not ":8080". The desk runs programs on request,
	// and auth is off unless someone turns it on, so a default that is
	// reachable from the network means anyone who can route to the box can
	// ask it to run a tool. Binding wider is a deliberate choice with a
	// warning attached, not something you get by typing nothing.
	addr := flag.String("addr", "127.0.0.1:8080",
		"listen address: host:port, or a path (containing /) for a unix socket created mode 0600 — "+
			"the only binding actually scoped to one OS user")
	toolsDir := flag.String("tools", "tools", "directory of tool folders; each folder must contain tcs.yaml")
	workerCount := flag.Int("workers", 10, "number of queued-execution workers")
	dataDir := flag.String("data", defaultDataDir(), "job workspace root (jobs/<id>/in, jobs/<id>/out live here, tail-able)")
	storeKind := flag.String("store", storeKindDefault, "durable job backend: sqlite (default) | postgres | memory")
	sqlitePath := flag.String("sqlite-path", sqlitePathDefault,
		"SQLite file for durable jobs + idempotency_key dedup (default: <data>/deskbox.db)")
	postgresDSN := flag.String("postgres-dsn", settings.PostgresDSN,
		"Postgres DSN for durable jobs + idempotency_key dedup (only used with -store=postgres; secret, keep it in .env, not deskbox.yaml)")
	strict := flag.Bool("strict", settings.Strict,
		"refuse to start, and refuse to run jobs, when sandboxing or per-job resource limits are unavailable, instead of degrading them to advisory")
	check := flag.Bool("check", false,
		"resolve every tool's declared dependencies against this box, print the result, and exit (0 if everything a tool needs is present and reachable from inside the sandbox)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	settings.Strict = *strict
	settings.ListenAddr = *addr

	if *showVersion {
		fmt.Println(versionString())
		return
	}

	if settings.AuthEnabled && settings.AuthToken == "" {
		log.Fatalf("DESKBOX_AUTH_ENABLED is set but DESKBOX_AUTH_TOKEN is empty")
	}
	// -check never serves, so the listener's auth posture is not part of the
	// question it answers.
	if !*check {
		if settings.AuthEnabled {
			log.Printf("auth: enabled — requests need Authorization: Bearer <token>")
		} else {
			log.Printf("auth: disabled — anything that can reach %s can submit jobs", *addr)
		}
	}

	// How the desk is being run decides what the sandbox is worth. Both of
	// these are operator choices rather than missing mechanisms, so they
	// are reported here and refused only under -strict.
	if !*check {
		log.Printf("user: tools will run as %s", describeUser())
		if runningAsRoot() {
			log.Printf("WARN: running as root — a tool runs as the same user the desk does, " +
				"so every job gets uid 0 inside its sandbox and leaves root-owned files behind. " +
				"Run the desk as an unprivileged user; that is what makes 'a tool can't touch " +
				"anything this account can't' true.")
		}
		if listensBeyondLoopback(*addr) && !settings.AuthEnabled {
			log.Printf("WARN: listening on %s with auth disabled — anything that can route to "+
				"this box can submit jobs to a daemon whose job is running programs. Set "+
				"DESKBOX_AUTH_ENABLED/DESKBOX_AUTH_TOKEN, or bind 127.0.0.1.", *addr)
		}
	}

	tools, err := LoadTools(*toolsDir)
	if err != nil {
		log.Fatalf("load tools: %v", err)
	}
	log.Printf("deskbox agent-desk %s: loaded %d tool(s) from %s", version, len(tools), *toolsDir)

	sandboxOK := hasBwrap()
	if sandboxOK {
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

	// Preflight: does this box actually have what the tools declare they
	// need, and can the sandbox see it? This is the difference between a
	// desk that fails on its first job with a bare ENOENT and one that says
	// "python3 is not installed" before it ever accepts a request.
	deps := Preflight(tools)
	problems := PreflightProblems(deps)
	if *check {
		fmt.Print(FormatPreflight(deps, sandboxOK))
		fatal := 0
		for _, d := range problems {
			if d.Fatal(sandboxOK) {
				fatal++
			}
		}
		switch {
		case fatal > 0:
			fmt.Printf("\n%d of %d %s unusable on this box.\n", fatal, len(deps),
				plural(len(deps), "dependency", "dependencies"))
			os.Exit(1)
		case len(problems) > 0:
			fmt.Printf("\n%d %s check out; %d warning(s) above.\n", len(deps),
				plural(len(deps), "dependency", "dependencies"), len(problems))
		case len(deps) == 0:
			fmt.Printf("no tool dependencies to check: %d tool(s) loaded from %s.\n",
				len(tools), *toolsDir)
		default:
			fmt.Printf("\nall %d %s check out.\n", len(deps), plural(len(deps), "dependency", "dependencies"))
		}
		return
	}
	if len(problems) > 0 {
		log.Printf("preflight: %d tool dependency problem(s) — run with -check for the full report:\n%s",
			len(problems), FormatPreflight(problems, sandboxOK))
	} else if len(deps) > 0 {
		log.Printf("preflight: all %d declared tool dependencies present and reachable inside the sandbox", len(deps))
	}

	// Every degradation above fails open: loudly logged, but open. -strict
	// turns them into a refusal, for a deployment where "the sandbox wasn't
	// available so we ran the tool anyway" is not an acceptable outcome.
	if settings.Strict {
		var missing []string
		if !sandboxOK {
			missing = append(missing, "bubblewrap (the sandbox itself)")
		}
		if !resourceLimitsOK {
			missing = append(missing, "systemd-run --user --scope (per-job memory and task caps)")
		}
		// Root is not a missing mechanism, it is a posture that erases what
		// the mechanisms buy: the sandbox still limits which paths exist,
		// but everything inside it is reached as uid 0. A desk that
		// advertises strict enforcement while handing every tool root is
		// making exactly the promise -strict exists to stop it making.
		if runningAsRoot() {
			missing = append(missing, "an unprivileged user to run tools as (the desk is running as root)")
		}
		if listensBeyondLoopback(*addr) && !settings.AuthEnabled {
			missing = append(missing, fmt.Sprintf(
				"either auth or a loopback bind (listening on %s with auth disabled)", *addr))
		}
		if len(missing) > 0 {
			log.Fatalf("-strict: refusing to start because enforcement would not actually "+
				"hold here: %s. Fix those, or drop -strict to run with the corresponding "+
				"guarantees downgraded to advisory.", strings.Join(missing, "; "))
		}
		log.Printf("strict: enabled — a job will fail rather than run with degraded enforcement")
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

	d := NewDesk(tools, q, *dataDir, settings, sandboxOK, resourceLimitsOK)

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
	mux.HandleFunc("GET /preflight", d.handlePreflight)

	var handler http.Handler = mux
	if settings.AuthEnabled {
		handler = requireAuth(settings.AuthToken, mux)
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	ln, err := listen(*addr)
	if err != nil {
		log.Fatalf("listen on %s: %v", *addr, err)
	}
	if isUnixSocket(*addr) {
		log.Printf("agent-desk listening on unix:%s (mode 0600 — reachable only by %s)",
			*addr, describeUser())
	} else {
		log.Printf("agent-desk listening on %s", *addr)
	}
	log.Fatal(srv.Serve(ln))
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
	s["enforcement"] = d.enforcement()
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
	if errors.Is(err, errQueueFull) {
		// Real backpressure, and an honest answer: the desk refuses fast
		// rather than blocking the caller's connection until a worker frees
		// up (what a bounded channel send used to do) or growing the
		// backlog without limit. Retry-After tells a well-behaved operator
		// to come back rather than hammer.
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
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
		// Every item already validated above, so this is a store failure or
		// a full queue partway through the fan-out — items before it are
		// already persisted and queued, not lost, and must not be. Return
		// their ids rather than discarding them: without batch_id/job_ids
		// here, a caller has no way to find and reconcile jobs that are
		// already running under a batch it was never told about, and its
		// only visible move (resubmit) would duplicate them (batch items
		// carry no idempotency key of their own).
		status := http.StatusInternalServerError
		if errors.Is(err, errQueueFull) {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "5")
		}
		writeJSON(w, status, map[string]any{
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

// handlePreflight reports the same dependency resolution -check prints, as
// JSON, so an operator (or an agent that just got a confusing tool failure)
// can ask a running desk what it is missing without shell access to the box.
func (d *Desk) handlePreflight(w http.ResponseWriter, r *http.Request) {
	deps := Preflight(d.tools)
	problems := 0
	unusable := 0
	for _, dep := range deps {
		if dep.Status != PreflightOK {
			problems++
		}
		if dep.Fatal(d.sandboxOK) {
			unusable++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandboxed":    d.sandboxOK,
		"path":         sandboxPath,
		"dependencies": deps,
		"problems":     problems,
		"unusable":     unusable,
		"ok":           unusable == 0,
	})
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
