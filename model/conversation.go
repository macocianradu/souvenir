package model

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

func EstimateTokens(messages []Message) int {
	tokens := 0
	for _, m := range messages {
		tokens += len(m.Content)/4 + 4
	}
	return tokens
}
