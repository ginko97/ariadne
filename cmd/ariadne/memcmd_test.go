package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ginko97/ariadne/internal/memory"
)

func runMemoryCommand(t *testing.T, store memory.Store, on bool, line string) (bool, string) {
	t.Helper()
	var out strings.Builder
	handled := memoryCommand(&out, store, on, line)
	return handled, out.String()
}

// With memory on, /remember keeps the person's exact words, marked as
// theirs; /memory numbers them; /forget n removes that one.
func TestChatMemoryCommandsSaveListAndForget(t *testing.T) {
	store := memory.Store{Path: filepath.Join(t.TempDir(), "MEMORY.md")}

	if _, out := runMemoryCommand(t, store, true, "/remember I live on Earth"); !strings.Contains(out, "saved to memory (1 / 50)") {
		t.Fatalf("/remember: %q", out)
	}
	if _, out := runMemoryCommand(t, store, true, "/remember i live on earth"); !strings.Contains(out, "already in memory") {
		t.Errorf("same fact again: %q", out)
	}
	if _, out := runMemoryCommand(t, store, true, "/remember Slides in English"); !strings.Contains(out, "(2 / 50)") {
		t.Errorf("second fact: %q", out)
	}
	notes, _ := store.Load()
	if len(notes) != 2 || notes[0].Text != "I live on Earth" || notes[0].RunID != memory.ByOperator {
		t.Fatalf("stored %+v", notes)
	}

	_, out := runMemoryCommand(t, store, true, "/memory")
	if !strings.Contains(out, " 1. I live on Earth\n    typed by you") || !strings.Contains(out, " 2. Slides in English") {
		t.Errorf("/memory:\n%s", out)
	}

	if _, out := runMemoryCommand(t, store, true, "/forget 1"); !strings.Contains(out, "forgot: I live on Earth") {
		t.Errorf("/forget 1: %q", out)
	}
	notes, _ = store.Load()
	if len(notes) != 1 || notes[0].Text != "Slides in English" {
		t.Errorf("after /forget 1: %+v", notes)
	}
	for line, want := range map[string]string{
		"/forget 5":   "no fact 5",
		"/forget one": "usage: /forget <n>",
		"/remember":   "usage: /remember <fact>",
	} {
		if _, out := runMemoryCommand(t, store, true, line); !strings.Contains(out, want) {
			t.Errorf("%s: %q, want %q", line, out, want)
		}
	}
}

// In a chat without -remember, /remember saves nothing and says how to turn
// memory on; the list and deleting still work, with a note that this chat
// does not use them.
func TestChatRememberRefusesWhenMemoryIsOff(t *testing.T) {
	store := memory.Store{Path: filepath.Join(t.TempDir(), "MEMORY.md")}

	_, out := runMemoryCommand(t, store, false, "/remember I live on Earth")
	if !strings.Contains(out, "memory is off in this chat") || !strings.Contains(out, "ariadne chat -remember") {
		t.Errorf("/remember with memory off: %q", out)
	}
	if notes, _ := store.Load(); len(notes) != 0 {
		t.Errorf("saved with memory off: %+v", notes)
	}
	if _, out := runMemoryCommand(t, store, false, "/memory"); !strings.Contains(out, "memory is off in this chat") {
		t.Errorf("/memory with memory off does not say so: %q", out)
	}
}

// Anything else is left to the chat loop: other commands, and messages.
func TestChatMemoryCommandsLeaveOtherLinesAlone(t *testing.T) {
	store := memory.Store{Path: filepath.Join(t.TempDir(), "MEMORY.md")}
	for _, line := range []string{"/model x", "/memorize", "remember that I like tea", "/rememberme"} {
		if handled, _ := runMemoryCommand(t, store, true, line); handled {
			t.Errorf("%q was taken as a memory command", line)
		}
	}
}

// /edit n rewrites fact n and keeps its number; bad input changes nothing.
func TestChatEditRewritesAFact(t *testing.T) {
	store := memory.Store{Path: filepath.Join(t.TempDir(), "MEMORY.md")}
	for _, text := range []string{"first", "I test v0.6.9"} {
		if err := store.Append(memory.Note{Text: text, RunID: memory.ByOperator}); err != nil {
			t.Fatal(err)
		}
	}
	if handled, out := runMemoryCommand(t, store, false, "/edit 2 I test v0.6.10"); !handled || !strings.Contains(out, "fact 2 now reads: I test v0.6.10") {
		t.Errorf("/edit 2: handled %v, %q", handled, out)
	}
	for line, want := range map[string]string{
		"/edit 9 x":              "no fact 9",
		"/edit two x":            "usage: /edit",
		"/edit 1":                "usage: /edit",
		"/edit 1 I TEST V0.6.10": "not changed:",
	} {
		if _, out := runMemoryCommand(t, store, false, line); !strings.Contains(out, want) {
			t.Errorf("%s: %q, want %q", line, out, want)
		}
	}
	if notes, _ := store.Load(); len(notes) != 2 || notes[0].Text != "first" || notes[1].Text != "I test v0.6.10" {
		t.Errorf("notes: %+v", notes)
	}
}
