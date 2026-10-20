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

func TestHeldCalls_OnlySinceTheLastUserMessage(t *testing.T) {
	call := func(id string) ToolCall { return ToolCall{Id: id, Function: FunctionCall{Name: "calendar_create"}} }
	messages := []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []ToolCall{call("old")}},
		{Role: "tool", ToolCallId: "old", Content: Held + "…"},
		{Role: "user", Content: "second"},
		{Role: "assistant", ToolCalls: []ToolCall{call("a"), call("b"), call("c")}},
		{Role: "tool", ToolCallId: "a", Content: Held + "…"},
		{Role: "tool", ToolCallId: "b", Content: "Created: …"},
		{Role: "tool", ToolCallId: "c", Content: Held + "…"},
		{Role: "tool", ToolCallId: "stray", Content: Held + "…"},
	}
	held := HeldCalls(messages)
	if len(held) != 2 || held[0].Call.Id != "a" || held[1].Call.Id != "c" || held[1].Result.ToolCallId != "c" {
		t.Fatalf("%+v", held)
	}
	if got := HeldCalls(append(messages, Message{Role: "user", Content: "never mind"})); len(got) != 0 {
		t.Fatalf("calls before the last user message are still held: %+v", got)
	}
}
