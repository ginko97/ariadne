package llm

import "encoding/json"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

type Block struct {
	Type BlockType `json:"type"`

	Text    string          `json:"text,omitempty"`     // text
	ID      string          `json:"id,omitempty"`       // tool_use
	Name    string          `json:"name,omitempty"`     // tool_use
	Args    json.RawMessage `json:"args,omitempty"`     // tool_use
	CallID  string          `json:"call_id,omitempty"`  // tool_result
	Content string          `json:"content,omitempty"`  // tool_result
	IsError bool            `json:"is_error,omitempty"` // tool_result
}

type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`

	// Model is the model that produced an assistant message, as the provider
	// reported serving it — which is not always the one that was asked for,
	// and not always the one the conversation is on now. A conversation can
	// switch models between turns, so "which model said this" is a property
	// of the message and cannot be recovered from the run.
	//
	// Empty on user and tool messages, and on assistant messages written by
	// a binary older than this field. Rendering treats empty as "unknown"
	// rather than as the current model, because guessing here would put a
	// name on an answer that never came from it.
	Model string `json:"model,omitempty"`
}
