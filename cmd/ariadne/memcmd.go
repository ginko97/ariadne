package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ginko97/ariadne/internal/memory"
)

// memoryCommand answers /memory, /remember and /forget in ariadne chat, and
// reports whether line was one of them. The terminal's counterpart of the
// page's memory panel, on the same store.
//
// /remember saves the person's own words without the model. It refuses in a
// chat started without -remember: the fact would be written to MEMORY.md and
// then reach no conversation that does not also turn memory on, which reads
// as "saved" and behaves as "ignored". /memory and /forget work either way,
// since looking after the file does not depend on this chat using it.
func memoryCommand(w io.Writer, store memory.Store, memoryOn bool, line string) bool {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "/memory":
		notes, err := store.Load()
		if err != nil {
			fmt.Fprintf(w, "! %v\n", err)
			return true
		}
		if len(notes) == 0 {
			fmt.Fprintln(w, "no remembered facts; /remember <fact> saves one")
		}
		for i, n := range notes {
			who := "typed by you"
			if n.RunID != memory.ByOperator {
				who = "from " + n.RunID
			}
			fmt.Fprintf(w, "%2d. %s\n    %s · %s\n", i+1, n.Text, who, n.At.Local().Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(w, "%d / %d facts in %s\n", len(notes), memory.MaxNotes, store.Path)
		if !memoryOn {
			fmt.Fprintln(w, "memory is off in this chat: these reach a conversation started with `ariadne chat -remember`")
		}
		return true

	case "/remember":
		if !memoryOn {
			fmt.Fprintln(w, "memory is off in this chat, so a fact saved here would reach no conversation;\n"+
				"start with `ariadne chat -remember` to use memory")
			return true
		}
		if arg == "" {
			fmt.Fprintln(w, "usage: /remember <fact>   saves the fact exactly as typed")
			return true
		}
		before, err := store.Load()
		if err != nil {
			fmt.Fprintf(w, "! %v\n", err)
			return true
		}
		if err := store.Append(memory.Note{Text: arg, RunID: memory.ByOperator}); err != nil {
			fmt.Fprintf(w, "not saved: %v\n", err)
			return true
		}
		after, err := store.Load()
		if err != nil {
			fmt.Fprintf(w, "! %v\n", err)
			return true
		}
		if len(after) == len(before) {
			fmt.Fprintln(w, "already in memory; nothing changed")
			return true
		}
		fmt.Fprintf(w, "saved to memory (%d / %d); new conversations start with it\n", len(after), memory.MaxNotes)
		return true

	case "/forget":
		n, err := strconv.Atoi(arg)
		if err != nil {
			fmt.Fprintln(w, "usage: /forget <n>   deletes fact n from /memory")
			return true
		}
		notes, err := store.Load()
		if err != nil {
			fmt.Fprintf(w, "! %v\n", err)
			return true
		}
		if n < 1 || n > len(notes) {
			fmt.Fprintf(w, "no fact %d; /memory lists %d\n", n, len(notes))
			return true
		}
		// By index and text together, as the page deletes: a fact that moved
		// since /memory was printed is refused rather than the wrong one gone.
		if err := store.Delete(n-1, notes[n-1].Text); err != nil {
			fmt.Fprintf(w, "! %v\n", err)
			return true
		}
		fmt.Fprintf(w, "forgot: %s\n", notes[n-1].Text)
		return true
	}
	return false
}
