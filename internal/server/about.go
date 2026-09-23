package server

import "net/http"

// ToolInfo is one tool a conversation here may call, as the page lists it.
type ToolInfo struct {
	Name string `json:"name"`
	// Asks is a card before every call. False means the tool runs as soon as
	// the model asks for it: a harmless built-in, or something -trust exempted.
	Asks bool `json:"asks"`
	MCP  bool `json:"mcp,omitempty"`
}

type aboutResponse struct {
	Version string     `json:"version"`
	Home    string     `json:"home,omitempty"`
	Tools   []ToolInfo `json:"tools"`
}

// handleAbout says which build this is and what a conversation here can do.
//
// The tools are fixed when `ariadne ui` starts — -exec, -trust, -allow and
// -mcp-config are flags, not page settings, so the riskiest choices stay one
// deliberate restart away rather than one click. What the page lacked was
// saying so: that exec is on, which MCP tools are loaded, what skips the card.
// Read-only, so GET, and no CSRF token: nothing here changes anything.
func (s *Server) handleAbout(w http.ResponseWriter, _ *http.Request) {
	tools := s.Tools
	if tools == nil {
		tools = []ToolInfo{}
	}
	writeJSON(w, http.StatusOK, aboutResponse{Version: s.Version, Home: s.Home, Tools: tools})
}
