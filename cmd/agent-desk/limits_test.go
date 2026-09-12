package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJobIDsAreWideEnough guards the birthday bound. The old ids were 4
// bytes: ~1% chance of a collision after 10,000 jobs, ~39% after 65,536.
// A collision is not cosmetic — the id is the store's primary key, the
// workspace path under data/jobs/<id>/, and the path segment in
// GET /jobs/{id}/out/{file}, so two jobs sharing one read and overwrite
// each other's output.
func TestJobIDsAreWideEnough(t *testing.T) {
	id := newJobID()
	hexDigits := len(strings.TrimPrefix(id, "job-"))
	if hexDigits < 32 {
		t.Fatalf("job id %q carries %d hex digits (%d bits); 128 bits is the floor that makes "+
			"collisions and enumeration both go away", id, hexDigits, hexDigits*4)
	}
	if !strings.HasPrefix(newBatchID(), "batch-") {
		t.Error("batch ids must stay distinguishable from job ids")
	}

	// Distinctness at a scale where the old width was already failing.
	const n = 200_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := newJobID()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q within %d draws", id, n)
		}
		seen[id] = struct{}{}
	}
}

// TestSubmitRejectsOversizeBody covers the unbounded-decode hole: the body
// was read into memory whatever its size, then copied again into the job
// workspace and again into the store — an unbounded multiple of itself in
// RSS, on a daemon that otherwise caps what a tool can hand back.
func TestSubmitRejectsOversizeBody(t *testing.T) {
	d := NewDesk(map[string]*Tool{"t": {Name: "t"}}, NewQueue(1, nil), t.TempDir(),
		&Settings{ListenAddr: "127.0.0.1:0"}, false, false)

	body := `{"input":{"blob":"` + strings.Repeat("A", maxRequestBody+1024) + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/tools/t", strings.NewReader(body))
	req.SetPathValue("name", "t") // the mux fills this in; httptest does not
	rec := httptest.NewRecorder()
	d.handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an oversize body must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "larger than") {
		t.Errorf("the refusal should say the body was too large, got %s", rec.Body.String())
	}
}

// TestSubmitRejectsTrailingContent: taking the first document and silently
// discarding the rest is how a client/desk disagreement turns into a bug
// long after the request that caused it.
func TestSubmitRejectsTrailingContent(t *testing.T) {
	d := NewDesk(map[string]*Tool{"t": {Name: "t"}}, NewQueue(1, nil), t.TempDir(),
		&Settings{ListenAddr: "127.0.0.1:0"}, false, false)

	req := httptest.NewRequest(http.MethodPost, "/tools/t",
		strings.NewReader(`{"input":{}} {"input":{"sneaky":true}}`))
	req.SetPathValue("name", "t")
	rec := httptest.NewRecorder()
	d.handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing content must be refused, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBatchRejectsTooManyItems: every item becomes its own job, row and
// workspace, so an accidental extra zero should get a clear refusal rather
// than being absorbed.
func TestBatchRejectsTooManyItems(t *testing.T) {
	d := NewDesk(map[string]*Tool{"t": {Name: "t"}}, NewQueue(1, nil), t.TempDir(),
		&Settings{ListenAddr: "127.0.0.1:0"}, false, false)

	var b strings.Builder
	fmt.Fprintf(&b, `{"tool":"t","items":[`)
	for i := 0; i < maxBatchItems+1; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	b.WriteString("]}")

	req := httptest.NewRequest(http.MethodPost, "/batches", strings.NewReader(b.String()))
	rec := httptest.NewRecorder()
	d.handleCreateBatch(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversize batch must be refused with 413, got %d: %s", rec.Code, rec.Body.String())
	}
}
