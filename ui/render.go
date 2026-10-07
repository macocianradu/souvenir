package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

var separatorStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("240")).
	MarginTop(1).
	MarginBottom(1)

func (m *uiModel) renderMessages() string {
	var b strings.Builder
	width := m.viewport.Width()
	entry := func(content string) {
		b.WriteString(content)
		b.WriteString("\n")
		b.WriteString(separatorStyle.Render(strings.Repeat("┈", width)))
		b.WriteString("\n")
	}

	for _, message := range m.conversation.Messages {
		switch message.Role {
		case "user":
			entry(lipgloss.PlaceHorizontal(width, lipgloss.Right, m.senderStyle.Render("You: ")+message.Content))
		case "assistant":
			entry(m.agentStyle.Render("Agent: ") + message.Content)
		}
	}

	if m.answerBuffer.Len() > 0 {
		entry(m.agentStyle.Render("Agent: ") + m.answerBuffer.String())
	} else if m.thinkingBuffer.Len() > 0 {
		entry(m.agentStyle.Render("Thinking: ") + m.thinkingBuffer.String())
	}
	return b.String()
}
