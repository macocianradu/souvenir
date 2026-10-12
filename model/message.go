package model

import (
	"encoding/base64"
	"encoding/json"
)

type Message struct {
	Id         string     `json:"-"`
	Seq        int        `json:"-"`
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     []Image    `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallId string     `json:"tool_call_id,omitempty"`
}

type Image struct {
	MediaType string
	Data      []byte
}

func (img Image) DataURL() string {
	return "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// A message with images is sent as a list of parts, text first.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	type imageURL struct {
		URL string `json:"url"`
	}
	type part struct {
		Type     string    `json:"type"`
		Text     string    `json:"text,omitempty"`
		ImageURL *imageURL `json:"image_url,omitempty"`
	}
	var parts []part
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
