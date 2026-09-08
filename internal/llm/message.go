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
}
