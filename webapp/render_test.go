package webapp

import (
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/model"
)

func TestMessageViews_RepliesAndToolCallsShowInTheOrderTheyRan(t *testing.T) {
	call := func(id, name string) []model.ToolCall {
		return []model.ToolCall{{Id: id, Type: "function", Function: model.FunctionCall{Name: name, Arguments: "{}"}}}
	}
	views := messageViews([]model.Message{
		{Role: "user", Content: "plan my day"},
		{Role: "assistant", Content: "Checking your calendar first.", ToolCalls: call("c1", "calendar_list")},
		{Role: "tool", ToolCallId: "c1", Content: "standup at 10"},
		{Role: "assistant", ToolCalls: call("c2", "calendar_list")},
		{Role: "tool", ToolCallId: "c2", Content: "nothing tomorrow"},
		{Role: "assistant", Content: "Now the weather.", ToolCalls: call("c3", "web_search")},
		{Role: "tool", ToolCallId: "c3", Content: "sunny"},
		{Role: "assistant", Content: "Standup at 10, and it will be sunny."},
	}, time.UTC)
	var got []string
	for _, v := range views {
		got = append(got, v.Role+":"+v.Content)
		for _, line := range v.Trail {
			got = append(got, "  "+line.Kind+":"+line.Text)
		}
	}
	want := []string{
		"user:plan my day",
		"assistant:Checking your calendar first.",
		"trail:",
		"  call:calendar_list {}",
		"  result:standup at 10",
		"  call:calendar_list {}",
		"  result:nothing tomorrow",
		"assistant:Now the weather.",
		"trail:",
		"  call:web_search {}",
		"  result:sunny",
		"assistant:Standup at 10, and it will be sunny.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("view %d is %q, want %q\nall: %q", i, got[i], want[i], got)
		}
	}
}
