package llm

import (
	"encoding/json"
	"strings"
)

type Content struct {
	Text     string
	Thinking string
}

func (c *Content) UnmarshalJSON(data []byte) error {
	*c = Content{}
	if string(data) == "null" {
		return nil
	}
	if json.Unmarshal(data, &c.Text) == nil {
		return nil
	}
	var chunks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(data, &chunks); err != nil {
		return err
	}
	var text, thinking strings.Builder
	for _, chunk := range chunks {
		switch chunk.Type {
		case "text":
			text.WriteString(chunk.Text)
		case "thinking":
			var inner Content
			if err := inner.UnmarshalJSON(chunk.Thinking); err != nil {
				return err
			}
			thinking.WriteString(inner.Text)
		}
	}
	c.Text, c.Thinking = text.String(), thinking.String()
	return nil
}
