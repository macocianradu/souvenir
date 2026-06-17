package model

type Message struct {
	Id		  string     `json:"-"`
	Seq	 	  int		 `json:"-"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}
