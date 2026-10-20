package model

import (
	"encoding/json"
	"testing"
)

func TestMarshal_ImagesBecomeContentParts(t *testing.T) {
	plain, _ := json.Marshal(Message{Role: "user", Content: "hi"})
	if string(plain) != `{"role":"user","content":"hi"}` {
		t.Errorf("plain message changed: %s", plain)
	}

	withImage, _ := json.Marshal(Message{Role: "user", Content: "what is this?", Images: []Image{{MediaType: "image/png", Data: []byte("png")}}})
	want := `{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,cG5n"}}]}`
	if string(withImage) != want {
		t.Errorf("got  %s\nwant %s", withImage, want)
	}

	onlyImage, _ := json.Marshal(Message{Role: "user", Images: []Image{{MediaType: "image/jpeg", Data: []byte("jpg")}}})
	if string(onlyImage) != `{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,anBn"}}]}` {
		t.Errorf("image only: %s", onlyImage)
	}
}

func TestMarshal_MistralThinkingGoesFirstAsAThinkingPart(t *testing.T) {
	got, _ := json.Marshal(Message{Role: "assistant", Content: "Checking", Thinking: "look it up",
		ToolCalls: []ToolCall{{Id: "call_1", Type: "function", Function: FunctionCall{Name: "search", Arguments: "{}"}}}})
	want := `{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{}"}}],` +
		`"content":[{"type":"thinking","thinking":[{"type":"text","text":"look it up"}]},{"type":"text","text":"Checking"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if plain, _ := json.Marshal(Message{Role: "assistant", Content: "hi"}.WithoutReasoning()); string(plain) != `{"role":"assistant","content":"hi"}` {
		t.Errorf("without reasoning: %s", plain)
	}
}
