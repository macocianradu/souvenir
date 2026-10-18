package model

import "time"

const (
	TitleSourceUser = "user"
	TitleSourceLLM  = "llm"
)

type Conversation struct {
	Id             string
	Title          string
	TitleSource    string
	Summary        string
	Preview        string
	UpdatedAt      time.Time
	Messages       []Message
	ContextSummary *ContextSummary
}

type ContextSummary struct {
	ThroughSeq int
	Content    string
}

func (c Conversation) Unsummarized() []Message {
	if c.ContextSummary == nil {
		return c.Messages
	}
	for i, m := range c.Messages {
		if m.Seq > c.ContextSummary.ThroughSeq {
			return c.Messages[i:]
		}
	}
	return nil
}

func (c Conversation) ContextMessages() []Message {
	if c.ContextSummary == nil {
		return c.Messages
	}
	summary := Message{Role: "system", Content: "Summary of the earlier part of this conversation:\n" + c.ContextSummary.Content}
	return append([]Message{summary}, c.Unsummarized()...)
}

// Images cost the model a few hundred to a few thousand tokens depending on
// their size; this is a middle guess.
const imageTokens = 1500

func EstimateTokens(messages []Message) int {
	tokens := 0
	for _, m := range messages {
		tokens += len(m.Content)/4 + 4 + len(m.Images)*imageTokens
		for _, call := range m.ToolCalls {
			tokens += (len(call.Function.Name)+len(call.Function.Arguments))/4 + 4
		}
	}
	return tokens
}

func Spoken(messages []Message) []Message {
	var out []Message
	for _, m := range messages {
		if m.Role == "tool" || (m.Role == "assistant" && m.Content == "") {
			continue
		}
		m.ToolCalls = nil
		out = append(out, m)
	}
	return out
}
