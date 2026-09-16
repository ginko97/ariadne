package loop

import (
	"fmt"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// Context compaction: drop the oldest work when the prompt outgrows its budget.
//
// The budget is measured in the provider's own prompt-token count, never a
// local tokenizer — the same rule the cost guard follows, and for the same
// reason: only the provider's number matches what the provider will do. The
// consequence is that compaction is necessarily *reactive*. Nothing can know a
// prompt is too large until a response comes back and says so, so a run crosses
// the line once and then trims. A tokenizer would let it be predictive and
// would also be wrong for every model whose BPE it does not implement.
const (
	// compactTarget is the fraction of the budget to trim down to. Trimming to
	// exactly the budget would put the run back over it on the next step and
	// compact again every step after that; the gap is hysteresis.
	compactTarget = 0.6

	// maxDigestChars bounds the replacement text, because the digest is itself
	// part of the prompt. A summary that grows without limit is just the
	// conversation again, more slowly.
	maxDigestChars = 1500
	maxResultChars = 100

	// maxTurnChars bounds a dropped question or answer. Longer than a tool
	// result, because a result is recognised from its ends while a sentence has
	// to survive as a sentence to be worth carrying at all.
	maxTurnChars = 160
)

// span is a half-open range of messages that has to be dropped together.
type span struct{ lo, hi int }

// isUserPrompt reports whether m is a message from the person rather than tool
// results.
func isUserPrompt(m llm.Message) bool {
	return m.Role == llm.RoleUser && !isToolResults(m)
}

// headLen is how many messages at the front are not droppable.
//
// Always the task: an agent that forgets what it was asked is not compacted, it
// is lobotomised.
//
// In a multi-turn conversation (more than one prompt from the person), it is
// the opening exchange: the task and the entire first turn's response up to the
// next prompt from the person. Protecting the opening exchange is what lets
// every subsequent unit be a complete turn (question, any tool calls, answer)
// that can be dropped without splitting tool pairs or orphaning answers.
//
// In a single-task run, message 1 is protected only when it is a plain assistant
// reply with no tool use. If it called tools, protecting it while its results stay
// droppable would orphan the call.
func headLen(msgs []llm.Message) int {
	for i := 1; i < len(msgs); i++ {
		if isUserPrompt(msgs[i]) {
			return i
		}
	}
	if len(msgs) >= 2 && msgs[1].Role == llm.RoleAssistant && !hasToolUse(msgs[1]) {
		return 2
	}
	return 1
}

// units groups a conversation into what can be dropped without corrupting it.
//
// In a multi-turn conversation, starting after the head above, each unit is a
// complete conversational turn: from a user prompt through all its tool calls
// and results to the assistant's reply. Dropping that unit drops the question,
// its tools and its answer together, leaving role alternation and tool pairings
// intact.
//
// In a single-task run, the unit is a tool round trip: an assistant tool_use and
// the user tool_result responding to it.
func units(msgs []llm.Message) []span {
	var out []span
	head := headLen(msgs)
	for i := head; i < len(msgs); {
		if isUserPrompt(msgs[i]) {
			j := i + 1
			for j < len(msgs) && !isUserPrompt(msgs[j]) {
				j++
			}
			out = append(out, span{i, j})
			i = j
			continue
		}
		if i+1 < len(msgs) && msgs[i].Role != msgs[i+1].Role {
			out = append(out, span{i, i + 2})
			i += 2
			continue
		}
		out = append(out, span{i, i + 1})
		i++
	}
	return out
}

func hasToolUse(m llm.Message) bool {
	for _, b := range m.Blocks {
		if b.Type == llm.BlockToolUse {
			return true
		}
	}
	return false
}

// size is a message's contribution to the prompt, in characters.
//
// Characters are a proxy for tokens and a bad one in general — but this only
// ever compares messages against other messages in the same conversation, to
// decide which ones to drop. The absolute number comes from the provider.
func size(m llm.Message) int {
	n := 8 // role and envelope, roughly
	for _, b := range m.Blocks {
		n += len(b.Text) + len(b.Args) + len(b.Content) + len(b.Name)
	}
	return n
}

func totalSize(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += size(m)
	}
	return n
}

// estimatedTokens is what the next prompt will cost, as best anything here can
// say before sending it.
//
// The provider's count is still the only measurement — but it measures the
// prompt that was sent, and tool results have landed since. So it is scaled by
// how much the conversation has grown since that measurement: characters used
// as a *ratio against the same conversation*, which is the argument size()
// already makes, rather than as an absolute token estimate, which would be the
// tokenizer this project refuses to have.
//
// Without this, a single step that fetches two large documents can push a run
// past the window while the recorded count still describes the prompt from
// before the fetch. Compaction then does nothing, the next request is too big
// to complete, and the run is checkpointed, resumable, and permanently stuck —
// which is exactly what dogfooding produced.
func estimatedTokens(s *State) int {
	if s.InputChars <= 0 || s.InputTokens <= 0 {
		return s.InputTokens
	}
	now := totalSize(s.Messages)
	if now <= s.InputChars {
		return s.InputTokens
	}
	return int(float64(s.InputTokens) * float64(now) / float64(s.InputChars))
}

// compact drops the oldest complete turns until the conversation should fit,
// and returns how many messages went.
//
// inputTokens is what the provider charged for the last prompt; budget is what
// this run is willing to spend on one. Both are token counts, and the ratio
// between them is the only thing taken from them — how much of the current
// conversation has to go.
func compact(s *State, inputTokens, budget int) int {
	if budget <= 0 || inputTokens <= budget || len(s.Messages) < 2 {
		return 0
	}

	have := totalSize(s.Messages)
	if have == 0 {
		return 0
	}
	// Proportional: if the prompt is twice the budget, roughly half of it has
	// to go, and compactTarget takes it below the line rather than onto it.
	want := int(float64(have) * compactTarget * float64(budget) / float64(inputTokens))

	all := units(s.Messages)
	// Keep the most recent exchange whatever the budget says. A conversation
	// trimmed to just the task is not a cheaper conversation, it is a restart,
	// and the run would loop repeating work it has already paid for.
	keepFloor := 1
	if len(all) <= keepFloor {
		return 0
	}

	drop := 0
	for drop < len(all)-keepFloor && have > want {
		u := all[drop]
		for i := u.lo; i < u.hi; i++ {
			have -= size(s.Messages[i])
		}
		drop++
	}
	if drop == 0 {
		return 0
	}

	head := headLen(s.Messages)
	cut := all[drop-1].hi
	dropped := s.Messages[head:cut]

	kept := make([]llm.Message, 0, head+len(s.Messages)-cut)
	kept = append(kept, s.Messages[:head]...)
	kept = append(kept, s.Messages[cut:]...)
	s.Messages = kept

	s.Dropped += len(dropped)
	mergeDigest(&s.Messages[0], digestLines(dropped), s.Dropped)
	return len(dropped)
}

// digestLines renders what was dropped, one line each, in the order it happened.
//
// Tool calls with their results, because that is the part of a dropped *task*
// the model needs later: what it already tried, and what came back. An
// assistant's prose alongside a call is still skipped — it was derived from the
// results, and the results are on the line above it.
//
// But a conversation contains no calls at all, and rendering only calls made a
// compacted chat produce a header announcing a list with nothing under it. The
// questions and answers were simply gone, with no record that they had been
// asked. So plain turns are recorded too: the person's messages always, and an
// assistant turn when it requested no tools and its words are therefore the
// only thing it contributed.
func digestLines(dropped []llm.Message) []string {
	results := map[string]string{}
	for _, m := range dropped {
		for _, blk := range m.Blocks {
			if blk.Type == llm.BlockToolResult {
				results[blk.CallID] = blk.Content
			}
		}
	}

	var lines []string
	for _, m := range dropped {
		calls := hasToolUse(m)
		for _, blk := range m.Blocks {
			switch {
			case blk.Type == llm.BlockToolUse:
				lines = append(lines, fmt.Sprintf("- %s(%s) -> %s",
					blk.Name, shorten(string(blk.Args), maxResultChars),
					shorten(results[blk.ID], maxResultChars)))
			case blk.Type != llm.BlockText || strings.TrimSpace(blk.Text) == "":
				// Tool results are already on the call's own line.
			case m.Role == llm.RoleUser:
				lines = append(lines, "- asked: "+shorten(blk.Text, maxTurnChars))
			case !calls:
				lines = append(lines, "- replied: "+shorten(blk.Text, maxTurnChars))
			}
		}
	}
	return lines
}

// CompactedCalls names the tools recorded in the digest: calls that were made
// but whose messages compaction has since dropped.
//
// The parse lives here, beside digestLines and mergeDigest, because the line
// format is this file's to change. A parser in another package would keep its
// own hand-written tests green through a format change while every compacted
// run it scored failed "never called" — which reads as the model getting worse.
//
// Block 1 of the task message is the digest whenever it exists; mergeDigest is
// the only writer. Lines past maxDigestChars have already been evicted, oldest
// first, so a long enough run can still lose a call from this list.
func (s *State) CompactedCalls() []string {
	if len(s.Messages) == 0 || len(s.Messages[0].Blocks) < 2 {
		return nil
	}
	digest := s.Messages[0].Blocks[1]
	if digest.Type != llm.BlockText {
		return nil
	}
	var names []string
	for _, l := range strings.Split(digest.Text, "\n") {
		rest, ok := strings.CutPrefix(l, "- ")
		if !ok || strings.HasPrefix(rest, "asked: ") || strings.HasPrefix(rest, "replied: ") {
			continue
		}
		if name, _, ok := strings.Cut(rest, "("); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

// shorten collapses whitespace and elides the middle, so one dropped call is
// one line.
//
// Both ends, not the first n characters. A tool result usually opens with
// something that identifies it and ends with the part that answered the
// question — a fetched page begins with a doctype and a calculation ends with
// the number. Clipping the head keeps the half that says least. This was a real
// bug found by a test asserting the digest carried the result: it did not, it
// carried two hundred characters of preamble.
//
// Runes rather than bytes: the content is arbitrary text and a byte slice can
// land in the middle of one.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := n * 2 / 3
	tail := n - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// mergeDigest rewrites the summary on the task message, keeping what earlier
// compactions recorded.
//
// It lives on the task message rather than in a message of its own because a
// separate message would have to pick a role, and either choice puts two turns
// of the same role next to each other — which OpenAI tolerates and other
// endpoints behind the same protocol may not. toWire joins a message's text
// blocks with a newline, so appending one keeps this exactly one user turn and
// leaves the alternation alone.
//
// Block 0 is always the task. Block 1, when present, is always this — so the
// second compaction rewrites the digest rather than stacking another behind it,
// and the run keeps one place to look.
//
// When the accumulated lines outgrow the cap the oldest go first. They are the
// ones the model is least likely to still need, and a digest that grows without
// limit is just the conversation again, more slowly.
func mergeDigest(task *llm.Message, newLines []string, total int) {
	var lines []string
	if len(task.Blocks) > 1 {
		old := strings.Split(task.Blocks[1].Text, "\n")
		for _, l := range old {
			if strings.HasPrefix(l, "- ") {
				lines = append(lines, l)
			}
		}
	}
	lines = append(lines, newLines...)

	header := fmt.Sprintf(
		"[%d earlier messages have been dropped to stay within the context budget. "+
			"What they contained:]", total)

	body := strings.Join(lines, "\n")
	for len(lines) > 1 && len(header)+1+len(body) > maxDigestChars {
		lines = lines[1:]
		body = strings.Join(lines, "\n")
	}

	blk := llm.Block{Type: llm.BlockText, Text: header + "\n" + body}
	if len(task.Blocks) > 1 {
		task.Blocks = append(task.Blocks[:1], blk)
		return
	}
	task.Blocks = append(task.Blocks, blk)
}
