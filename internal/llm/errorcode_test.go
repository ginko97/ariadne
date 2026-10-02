package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenRouter sends error.code as a number, OpenAI as a string. Either way the
// person must be shown the provider's message, not a decoding error.
func TestErrorCodeOfEitherTypeKeepsTheMessage(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Provider returned error","code":502}}`,
		`{"error":{"message":"Provider returned error","code":"server_error"}}`,
	} {
		_, err := fromWire([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "Provider returned error") {
			t.Errorf("fromWire(%s) = %v, want the provider's message", body, err)
		}
	}
	_, err := fromWire([]byte(`{"error":{"message":"too long","code":"context_length_exceeded"}}`))
	if !errors.Is(err, ErrContextLength) {
		t.Errorf("a string context_length_exceeded code: %v, want ErrContextLength", err)
	}
}

func TestStreamErrorWithNumericCodeKeepsTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"upstream overloaded\",\"code\":502}}\n\n"))
	}))
	defer srv.Close()
	o := NewOpenAI("k", WithBaseURL(srv.URL))
	seq, err := o.Stream(context.Background(), Request{Model: "m",
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for _, err := range seq {
		if err != nil {
			got = err
		}
	}
	if got == nil || !strings.Contains(got.Error(), "upstream overloaded") {
		t.Errorf("stream error = %v, want the provider's message", got)
	}
}
