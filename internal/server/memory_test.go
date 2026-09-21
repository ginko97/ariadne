package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ginko97/ariadne/internal/memory"
)

func TestMemoryListEmpty(t *testing.T) {
	_, ts := newTestServer(t)
	// Without memory store configured
	resp, err := http.Get(ts.URL + "/api/memory")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var items []memoryItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want 0", len(items))
	}
}

func TestMemoryListAndCSRFProtection(t *testing.T) {
	s, ts := newTestServer(t)
	memPath := filepath.Join(t.TempDir(), "MEMORY.md")
	memStore := memory.Store{Path: memPath}
	s.MemoryStore = memStore

	// Append two notes
	if err := memStore.Append(memory.Note{Text: "first fact", RunID: "run_1", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := memStore.Append(memory.Note{Text: "second fact", RunID: "run_2", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// GET /api/memory requires no CSRF token (read-only)
	resp, err := http.Get(ts.URL + "/api/memory")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/memory status = %d, want 200", resp.StatusCode)
	}

	var items []memoryItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Text != "first fact" || items[0].RunID != "run_1" || items[0].Index != 0 {
		t.Errorf("unexpected item 0: %+v", items[0])
	}
	if items[1].Text != "second fact" || items[1].RunID != "run_2" || items[1].Index != 1 {
		t.Errorf("unexpected item 1: %+v", items[1])
	}

	// DELETE /api/memory without CSRF token must fail with 403
	delReq, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/memory", strings.NewReader(`{"index":0,"text":"first fact"}`))
	if err != nil {
		t.Fatal(err)
	}
	delReq.Header.Set("Content-Type", "application/json")
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE without CSRF status = %d, want 403 Forbidden", delResp.StatusCode)
	}

	// DELETE /api/memory with wrong expected text must fail with 400
	delReq2, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/memory", strings.NewReader(`{"index":0,"text":"wrong text"}`))
	if err != nil {
		t.Fatal(err)
	}
	delReq2.Header.Set("Content-Type", "application/json")
	delReq2.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	delResp2, err := http.DefaultClient.Do(delReq2)
	if err != nil {
		t.Fatal(err)
	}
	delResp2.Body.Close()
	if delResp2.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE with mismatched text status = %d, want 400 Bad Request", delResp2.StatusCode)
	}

	// DELETE /api/memory with valid CSRF and matching text must succeed
	delReq3, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/memory", strings.NewReader(`{"index":0,"text":"first fact"}`))
	if err != nil {
		t.Fatal(err)
	}
	delReq3.Header.Set("Content-Type", "application/json")
	delReq3.Header.Set("X-Ariadne-CSRF", s.CSRFToken)
	delResp3, err := http.DefaultClient.Do(delReq3)
	if err != nil {
		t.Fatal(err)
	}
	delResp3.Body.Close()
	if delResp3.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 OK", delResp3.StatusCode)
	}

	// Verify note was deleted from store
	notes, err := memStore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].Text != "second fact" {
		t.Fatalf("unexpected notes remaining: %+v", notes)
	}
}

func TestChatEnablesMemoryWhenStoreConfigured(t *testing.T) {
	s, ts := newTestServer(t, endResponse("hello"))
	memPath := filepath.Join(t.TempDir(), "MEMORY.md")
	s.MemoryStore = memory.Store{Path: memPath}

	resp := post(t, s, ts, `{"message":"hi"}`, nil)
	evs := events(t, bodyOf(t, resp))
	runID, _ := evs[len(evs)-1].Data["run_id"].(string)
	if runID == "" {
		t.Fatal("done event carried no run id")
	}

	// Verify checkpoint has state.Memory = true
	st, err := s.Store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Memory {
		t.Error("state.Memory is false, want true")
	}
}
