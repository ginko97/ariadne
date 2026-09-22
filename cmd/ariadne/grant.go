package main

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/ginko97/ariadne/internal/llm"
	"github.com/ginko97/ariadne/internal/tool"
)

// webFetchGrantKey is the policy for turn-scoped grants: which calls the
// operator may allow once for the rest of a turn, and under what key.
//
// Only web_fetch, and only by origin — scheme, host and port together. The
// gate on web_fetch exists because a URL carries data out, so the question an
// approval really answers is "may data go *there*", and the destination is the
// origin. The scheme is part of it because a grant for https://github.com must
// not cover http://github.com, where the same URL travels in plaintext. The
// host is lowercased and loses a trailing dot so that spellings of one
// destination share a grant; the port is kept only when it is explicit.
//
// Nothing else is grantable. write_file and edit_file keep one card per
// change because each card shows a different diff; exec never gets a
// shortcut; an MCP tool's name says nothing about where it sends data.
func webFetchGrantKey(c llm.ToolCall) (string, bool) {
	if c.Name != tool.WebFetchName {
		return "", false
	}
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(c.Args, &in); err != nil {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", false
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return scheme + "://" + host, true
}
