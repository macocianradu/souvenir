package model

import "slices"

type ToolCall struct {
	Id       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

const notRun = "The tool was not run: the turn was stopped first."

func AnswerToolCalls(messages []Message) []Message {
	var out []Message
	var open []string
	answer := func() {
		for _, id := range open {
			out = append(out, Message{Role: "tool", ToolCallId: id, Content: notRun})
		}
		open = nil
	}
	for _, m := range messages {
		if m.Role == "tool" {
			if i := slices.Index(open, m.ToolCallId); i >= 0 {
				open = slices.Delete(open, i, i+1)
				out = append(out, m)
			}
			continue
		}
		answer()
		out = append(out, m)
		for _, call := range m.ToolCalls {
			open = append(open, call.Id)
		}
	}
	answer()
	return out
}

func (m Message) IsToolRound() bool {
	return m.Role == "tool" || len(m.ToolCalls) > 0
}
