package main

import (
	"context"
	"strings"

	"github.com/ginko97/ariadne/internal/loop"
)

// draftSystemPrompt is what the model is told when it drafts a task file.
const draftSystemPrompt = `You are drafting a task file for ariadne, a personal assistant. A task file is a markdown document of instructions that the person will read, change, and then run; when it runs, the model treats every line as the person's own instruction.

Write only the task file, in markdown, with nothing before or after it.

Include:
- a # title;
- what the task is for, in a sentence or two;
- where to look: name the official sources or sites to read, but do not invent exact page URLs;
- the steps;
- what the result should look like: sections, a table where one helps, and a list of the sources used with their URLs;
- rules: read the sources rather than answering from memory, cite every source, say where sources disagree, never guess a URL, and do not rank or recommend unless the description asks for it.

End with the line: Give the result as your answer.

You have no tools and read nothing while drafting. Write from the description alone, and mark anything you would need to know with TODO for the person to fill in.`

// noToolsWhileDrafting is the drafting agent's whole allow-list. No tool has
// this name, so a tool call the model makes anyway is refused by the loop.
const noToolsWhileDrafting = "(none while drafting a task)"

// draftTask asks the model for a task file matching description, as one
// conversation (checkpointed and traced like any other, so the draft is on
// record) that is offered no tools. A task file runs as the person's own,
// unfenced instruction; a draft written by a model that had just read a web
// page or a file could carry that page's instructions into every later run.
// With nothing offered, there is nothing it could have read.
func draftTask(ctx context.Context, opts agentOpts, folder, description string) (string, error) {
	opts.Workspace = folder
	a := newAgentFor(opts)
	a.System = draftSystemPrompt
	// The whole allow-list: the loop offers only allowed tools, so the
	// request carries none, and refuses a call the model makes anyway.
	a.Allow = []string{noToolsWhileDrafting}
	a.MaxSteps = 2

	state := loop.NewState(opts.RunID, "Draft a task file: "+description)
	state.Workspace = folder
	state.Memory = opts.Memory
	answer, err := a.Run(ctx, state)
	if err != nil {
		return "", err
	}
	return unfence(answer), nil
}

// unfence removes one code fence a model may put around the whole file
// ("```markdown ... ```"), which would otherwise be saved into it.
func unfence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s + "\n"
	}
	body := strings.TrimSuffix(s, "```")
	if i := strings.Index(body, "\n"); i >= 0 {
		body = body[i+1:]
	} else {
		return s + "\n"
	}
	return strings.TrimSpace(body) + "\n"
}
