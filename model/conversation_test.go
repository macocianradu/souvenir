package model

import "testing"

func conversation(n int) Conversation {
	var c Conversation
	for i := 1; i <= n; i++ {
		c.Messages = append(c.Messages, Message{Role: "user", Content: "message", Seq: i})
	}
	return c
}

func TestContextMessages_WithoutSummarySendsTheFullHistory(t *testing.T) {
	c := conversation(3)
	if got := c.ContextMessages(); len(got) != 3 || got[0].Seq != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestContextMessages_WithSummarySendsItThenOnlyLaterMessages(t *testing.T) {
	c := conversation(5)
	c.ContextSummary = &ContextSummary{ThroughSeq: 3, Content: "earlier"}
	got := c.ContextMessages()
	if len(got) != 3 || got[0].Role != "system" || got[1].Seq != 4 || got[2].Seq != 5 {
		t.Fatalf("%+v", got)
	}
	if len(c.Messages) != 5 {
		t.Fatal("history was modified")
	}
}

func TestUnsummarized_IsEmptyWhenTheSummaryCoversEverything(t *testing.T) {
	c := conversation(2)
	c.ContextSummary = &ContextSummary{ThroughSeq: 2}
	if got := c.Unsummarized(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestEstimateTokens_CountsAboutFourCharactersPerToken(t *testing.T) {
	msgs := []Message{{Content: string(make([]byte, 400))}, {Content: ""}}
	if got := EstimateTokens(msgs); got != 108 {
		t.Fatal(got)
	}
}
