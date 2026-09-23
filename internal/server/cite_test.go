package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/loop"
)

// The turn's end says which cited URLs nothing opened, so the page can say so
// under the answer that cited them.
func TestDoneEventNamesUnopenedCitations(t *testing.T) {
	s, ts := newTestServer(t, endResponse("Per https://never.example/report, QRIS is free."))

	resp := post(t, s, ts, `{"message":"is QRIS free?"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, bodyOf(t, resp))
	}
	evs := events(t, bodyOf(t, resp))
	done := evs[len(evs)-1]
	if done.Name != "done" {
		t.Fatalf("last event = %q, want done", done.Name)
	}
	got, _ := done.Data["unopened"].([]any)
	if len(got) != 1 || got[0] != "https://never.example/report" {
		t.Fatalf("unopened = %v, want the one cited URL", done.Data["unopened"])
	}
}

// A reloaded conversation shows the same notes it showed live: each on the
// last answer of the turn that cited the URLs, the last turn included, and
// none on an answer that is not its turn's last.
func TestTranscriptPutsUnopenedOnEachTurnsLastAnswer(t *testing.T) {
	s, ts := newTestServer(t)

	st := loop.NewState("run_cites", "compare the banks")
	st.Messages = append(st.Messages,
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "Fetching."},
			{Type: llm.BlockToolUse, ID: "c1", Name: "web_fetch", Args: json.RawMessage(`{"url":"https://bri.example/qris"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{
			{Type: llm.BlockToolResult, CallID: "c1", Content: "<untrusted source=\"web_fetch\">\nURL: https://bri.example/qris\n\ntext\n</untrusted>"},
		}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "BRI: https://bri.example/qris. BCA: https://bca.example/guessed."},
		}},
		llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{{Type: llm.BlockText, Text: "and BRI again?"}}},
		llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
			{Type: llm.BlockText, Text: "As before, https://bri.example/qris; Mandiri per https://mandiri.example/qris."},
		}},
	)
	if err := s.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	resp, got := getTranscript(t, ts, "run_cites")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var notes [][]string
	for _, e := range got.Messages {
		if e.Kind == "answer" {
			notes = append(notes, e.Unopened)
		}
	}
	want := [][]string{nil, {"https://bca.example/guessed"}, {"https://mandiri.example/qris"}}
	if !reflect.DeepEqual(notes, want) {
		t.Fatalf("unopened per answer = %q, want %q", notes, want)
	}
}
