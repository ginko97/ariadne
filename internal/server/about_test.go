package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// The page reads the build and the tool list from here; an empty list must
// arrive as [] so the page can tell "no tools" from "no answer".
func TestAboutReportsTheBuildAndTheTools(t *testing.T) {
	s, ts := newTestServer(t)
	get := func() aboutResponse {
		t.Helper()
		resp, err := ts.Client().Get(ts.URL + "/api/about")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["tools"]) == "null" {
			t.Error("tools arrived as null, not []")
		}
		var a aboutResponse
		_ = json.Unmarshal(raw["version"], &a.Version)
		_ = json.Unmarshal(raw["tools"], &a.Tools)
		return a
	}

	get() // no tools set: must still be a list

	s.Version = "v0.6.5"
	s.Tools = []ToolInfo{{Name: "calc"}, {Name: "exec", Asks: true}, {Name: "fs__read_text_file", MCP: true}}
	got := get()
	if got.Version != "v0.6.5" || !reflect.DeepEqual(got.Tools, s.Tools) {
		t.Errorf("got %+v, want version v0.6.5 and %+v", got, s.Tools)
	}
}
