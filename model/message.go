package model

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

type Message struct {
	Id               string     `json:"-"`
	Seq              int        `json:"-"`
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	Images           []Image    `json:"-"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallId       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Reasoning        string     `json:"reasoning,omitempty"`
	Thinking         string     `json:"-"`
	CreatedAt        time.Time  `json:"-"`
}

func (m Message) WithoutReasoning() Message {
	m.ReasoningContent, m.Reasoning, m.Thinking = "", "", ""
	return m
}

type Image struct {
	MediaType string
	Data      []byte
}

func (img Image) DataURL() string {
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// A message with images or Mistral thinking is sent as a list of parts:
// thinking, then text, then images.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 && m.Thinking == "" {
		return json.Marshal(plain(m))
	}
	type imageURL struct {
		URL string `json:"url"`
	}
	type part struct {
		Type     string    `json:"type"`
		Text     string    `json:"text,omitempty"`
		Thinking []part    `json:"thinking,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	var parts []part
	if m.Thinking != "" {
		parts = append(parts, part{Type: "thinking", Thinking: []part{{Type: "text", Text: m.Thinking}}})
	}
	if m.Content != "" {
		parts = append(parts, part{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, part{Type: "image_url", ImageURL: &imageURL{URL: img.DataURL()}})
	}
	return json.Marshal(struct {
		plain
		Content []part `json:"content"`
	}{plain(m), parts})
}
