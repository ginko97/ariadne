// Package cite checks a conversation's citations against what it opened.
//
// A research answer names its sources, and a model will name a URL it never
// opened: one it saw in a search result, one it remembered, one it built from
// a date. Every report checked by hand in the first week of daily use had at
// least one. No second model is needed to find them — the conversation holds
// every tool call and its result, so "was this URL opened" is a lookup.
//
// What this does not check: whether the page said what the answer claims it
// said. A cited URL that was opened can still be misquoted.
package cite

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
)

// Unopened returns the URLs cited in the conversation's last turn that no
// tool call in the conversation opened, in the order they first appear.
func Unopened(msgs []llm.Message) []string {
	turns := Turns(msgs)
	if len(turns) == 0 {
		return nil
	}
	t := turns[len(turns)-1]
	return Check(msgs, t[0], t[1])
}

// Turns splits a conversation into turns: [start, end) index pairs, each
// starting at a message the person typed. Messages before the first one
// (a system prompt) belong to no turn.
func Turns(msgs []llm.Message) [][2]int {
	var out [][2]int
	for i, m := range msgs {
		if m.Role == llm.RoleUser && hasText(m) {
			if n := len(out); n > 0 {
				out[n-1][1] = i
			}
			out = append(out, [2]int{i, len(msgs)})
		}
	}
	return out
}

// Check returns the URLs cited in msgs[start:end] — in the model's prose and
// in what it wrote to files through a call that succeeded — that no call in
// msgs[:end] opened. A page opened in an earlier turn counts; one opened
// after the answer does not.
//
// Opened means a tool call with a "url" argument whose result was not an
// error — web_fetch, or an MCP tool that takes a url — or the final address
// web_fetch reports after following a redirect. A fetch that returned 404
// opened nothing.
func Check(msgs []llm.Message, start, end int) []string {
	failed := map[string]bool{} // call id -> the result was an error
	for _, m := range msgs[:end] {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult {
				failed[b.CallID] = b.IsError
			}
		}
	}

	opened := map[string]bool{}
	for _, m := range msgs[:end] {
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockToolUse:
				if f, ok := failed[b.ID]; ok && !f {
					if u := stringArg(b.Args, "url"); u != "" {
						opened[key(u)] = true
					}
				}
			case llm.BlockToolResult:
				if !b.IsError {
					if m := finalURL.FindStringSubmatch(b.Content); m != nil {
						opened[key(m[1])] = true
					}
				}
			}
		}
	}

	var out []string
	seen := map[string]bool{}
	cite := func(text string) {
		for _, u := range Find(text) {
			k := key(u)
			if opened[k] || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, u)
		}
	}
	for _, m := range msgs[start:end] {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Blocks {
			switch b.Type {
			case llm.BlockText:
				cite(b.Text)
			case llm.BlockToolUse:
				if f, ok := failed[b.ID]; !ok || f {
					continue // never ran, denied, or failed: nothing was written
				}
				switch b.Name {
				case "write_file":
					cite(stringArg(b.Args, "content"))
				case "edit_file":
					cite(stringArg(b.Args, "new_text"))
				}
			}
		}
	}
	return out
}

// finalURL is the first line of a web_fetch result: where the page was read
// from, after any redirect it followed. Only web_fetch's fence and only its
// first line, which the tool writes before any page text: a page that prints
// "URL: ..." in its body, or a file that starts with it, opened nothing.
var finalURL = regexp.MustCompile(`\A<untrusted source="web_fetch">\nURL: (\S+)\n`)

// urlPattern is an http(s) URL as it appears in prose or markdown. It stops at
// whitespace, quotes, angle brackets and the closing bracket of a markdown
// link; trailing punctuation is trimmed after.
var urlPattern = regexp.MustCompile("https?://[^\\s<>\"'`\\]|]+")

// Find returns the http and https URLs in text, as written, trailing
// punctuation removed. A bare domain ("bankmandiri.co.id") is not a URL and is
// not returned: it names a site, not a page anyone could have opened.
func Find(text string) []string {
	var out []string
	for _, u := range urlPattern.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:!?*_")
		// A ")" closes a markdown link or a parenthesis around the URL unless
		// the URL opened one itself, as Wikipedia's do.
		for strings.HasSuffix(u, ")") && strings.Count(u, "(") < strings.Count(u, ")") {
			u = strings.TrimRight(strings.TrimSuffix(u, ")"), ".,;:!?*_")
		}
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			out = append(out, u)
		}
	}
	return out
}

// key is what two spellings of one page have in common: host case, a leading
// "www.", http against https, a fragment and a trailing slash do not make a
// different page. The query does.
func key(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	k := host + path
	if u.RawQuery != "" {
		k += "?" + u.RawQuery
	}
	return k
}

func stringArg(args json.RawMessage, name string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[name], &s) != nil {
		return ""
	}
	return s
}

func hasText(m llm.Message) bool {
	for _, b := range m.Blocks {
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}
