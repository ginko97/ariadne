package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The other half: what a tool does when the arguments do not parse.
//
// An IsError result, not a returned error. "The model sent nonsense" is
// something it can fix on the next step; "the tool could not be reached" is
// not, and conflating them turns a recoverable mistake into a dead run.
func TestToolsRejectMalformedArgumentsRecoverably(t *testing.T) {
	dir := t.TempDir()
	bad := json.RawMessage(`{"expr": "240*0.15"`) // one brace short, from the fixture

	for _, tl := range []Tool{Calc{}, NewFetch(dir), NewWriteFile(dir)} {
		res, err := tl.Call(context.Background(), "call_1", bad)
		if err != nil {
			t.Errorf("%s returned an error rather than an IsError result: %v", tl.Name(), err)
			continue
		}
		if !res.IsError {
			t.Errorf("%s accepted malformed arguments: %q", tl.Name(), res.Content)
		}
		if !strings.Contains(res.Content, tl.Name()) {
			t.Errorf("%s: the message should name the tool: %q", tl.Name(), res.Content)
		}
	}
}
