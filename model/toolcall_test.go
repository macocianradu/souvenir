package model

import (
	"slices"
	"testing"
)

func roles(messages []Message) []string {
	var out []string
	for _, m := range messages {
		out = append(out, m.Role+":"+m.ToolCallId)
	}
	return out
}

func TestAnswerToolCalls_FillsMissingResultsAndDropsOrphans(t *testing.T) {
	request := Message{Role: "assistant", ToolCalls: []ToolCall{{Id: "a"}, {Id: "b"}}}
	got := AnswerToolCalls([]Message{
		{Role: "tool", ToolCallId: "folded", Content: "its request was summarized away"},
		{Role: "user", Content: "q"},
		request,
		{Role: "tool", ToolCallId: "a", Content: "done"},
		{Role: "user", Content: "q2"},
		request,
	})
	want := []string{"user:", "assistant:", "tool:a", "tool:b", "user:", "assistant:", "tool:a", "tool:b"}
	if !slices.Equal(roles(got), want) {
		t.Fatalf("got %v\nwant %v", roles(got), want)
	}
	if got[2].Content != "done" || got[3].Content != notRun {
		t.Fatalf("results: %q, %q", got[2].Content, got[3].Content)
	}
}
