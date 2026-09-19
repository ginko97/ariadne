package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/ginko97/ariadne/internal/tool"
)

// Separator joins a server name to the tools it offers: "fs" offering
// "write_file" is the tool "fs__write_file".
//
// Every MCP tool is prefixed, not only ones that collide. The reference
// filesystem server offers write_file, which a built-in already has; prefixing
// on collision alone would mean a built-in added later silently renames an MCP
// tool, and every -approve list naming it would stop matching. Dots are not
// allowed in tool names by OpenAI-compatible APIs, and "__" is the convention
// other MCP clients already use.
const Separator = "__"

var (
	serverName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// toolName is what OpenAI-compatible endpoints accept. A name outside it
	// is refused at connect, naming the server and tool, rather than by the
	// provider as a 400 on the first turn that names neither.
	toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Config is the file that says which MCP servers to start.
//
// The shape is the one the ecosystem already uses — a "mcpServers" object keyed
// by name, each with a command and its arguments — so a config written for any
// other MCP client works here unchanged. Inventing a different one would mean
// every user translating a file they already have.
//
//	{
//	  "mcpServers": {
//	    "filesystem": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "/srv/docs"]},
//	    "git":        {"command": "uvx", "args": ["mcp-server-git", "--repository", "/srv/repo"]}
//	  }
//	}
type Config struct {
	Servers map[string]ServerSpec `json:"mcpServers"`
}

// ServerSpec is one subprocess to start and speak MCP to over its stdio.
type ServerSpec struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Disabled keeps an entry in the file without starting it, which is what
	// people actually do with these configs rather than deleting and retyping.
	Disabled bool `json:"disabled,omitempty"`
}

// LoadConfig reads a config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("mcp: parse %s: %w", path, err)
	}
	for name, s := range c.Servers {
		if s.Command == "" {
			return nil, fmt.Errorf("mcp: server %q has no command", name)
		}
		// The name becomes the prefix of every tool the server offers, so it has
		// to be legal inside a tool name, and it cannot contain the separator or
		// "a__b" offering "c" and "a" offering "b__c" would be the same tool.
		if !serverName.MatchString(name) || strings.Contains(name, Separator) {
			return nil, fmt.Errorf("mcp: server name %q must be letters, digits, _ or - and not contain %q, "+
				"because it prefixes every tool the server offers", name, Separator)
		}
	}
	return &c, nil
}

// Connect starts every enabled server and collects the tools they offer.
//
// The returned close function stops all of them; it is safe to call with no
// servers running. Servers are dialled in name order so a failure reports the
// same one every time rather than whichever subprocess lost a race.
//
// A server that will not start fails the whole call rather than being skipped
// with a warning. An agent quietly missing the tools it was configured with
// looks exactly like a model that will not use them, and this project has
// already paid for that confusion once — the read side of memory enabled with a
// dead write side.
func Connect(ctx context.Context, cfg *Config) (tools []tool.Tool, closeAll func(), err error) {
	if cfg == nil || len(cfg.Servers) == 0 {
		return nil, func() {}, nil
	}

	var started []*Server
	closeAll = func() {
		for _, s := range started {
			_ = s.Close()
		}
	}

	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		spec := cfg.Servers[name]
		if spec.Disabled {
			continue
		}

		srv, err := Dial(ctx, spec.Command, spec.Args...)
		if err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("mcp: server %q: %w", name, err)
		}
		started = append(started, srv)

		ts, err := srv.Tools(ctx)
		if err != nil {
			closeAll()
			return nil, func() {}, fmt.Errorf("mcp: server %q: %w", name, err)
		}
		for _, tl := range ts {
			rt, ok := tl.(*remoteTool)
			if !ok {
				continue
			}
			rt.name = name + Separator + rt.remote
			if !toolName.MatchString(rt.name) {
				closeAll()
				return nil, func() {}, fmt.Errorf("mcp: server %q: tool %q becomes %q, which is not a legal tool name "+
					"(letters, digits, _ or -, at most 64); shorten the server name", name, rt.remote, rt.name)
			}
		}
		tools = append(tools, ts...)
	}
	return tools, closeAll, nil
}

// CheckCollisions refuses a remote tool whose name a local one already has.
//
// tool.New keeps the last tool with a given name and says nothing, so a server
// offering "fetch" would replace the sandboxed one — swapping a tool confined by
// os.Root for a remote one that answers to somebody else's process. That is a
// silent privilege change, and the kind that is invisible until it matters, so
// the collision is an error the operator resolves rather than something decided
// here by ordering.
func CheckCollisions(local []tool.Tool, remote []tool.Tool) error {
	have := make(map[string]bool, len(local))
	for _, t := range local {
		have[t.Name()] = true
	}
	seen := make(map[string]bool, len(remote))
	for _, t := range remote {
		switch {
		case have[t.Name()]:
			return fmt.Errorf("mcp: a server offers %q, which is already a built-in tool; "+
				"rename that server in the config or disable it", t.Name())
		case seen[t.Name()]:
			return fmt.Errorf("mcp: two servers both offer %q; disable one of them", t.Name())
		}
		seen[t.Name()] = true
	}
	return nil
}
