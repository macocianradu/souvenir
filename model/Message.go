package model

type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	FunctionName      string `json:"function"`
	FunctionArguments string `json:"arguments"`
}
