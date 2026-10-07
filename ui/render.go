package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

var separatorStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("240")).
	MarginTop(1).
	MarginBottom(1)

var thinkingStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("245")).
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

	thinking := func() {
		b.WriteString(thinkingStyle.Width(width).Render("Thinking: " + m.thinkingBuffer.String()))
		b.WriteString("\n")
	}

	messages := m.conversation.Messages
	thinkingFor := -1
	if !m.streaming && m.thinkingBuffer.Len() > 0 {
		if last := len(messages) - 1; last >= 0 && messages[last].Role == "assistant" {
			thinkingFor = last
		}
	}

	for i, message := range messages {
		switch message.Role {
		case "user":
			entry(lipgloss.PlaceHorizontal(width, lipgloss.Right, m.senderStyle.Render("You: ")+message.Content))
		case "assistant":
			if i == thinkingFor {
				thinking()
			}
			entry(m.agentStyle.Render("Agent: ") + message.Content)
		}
	}

	if m.streaming && m.thinkingBuffer.Len() > 0 {
		thinking()
	}
	if m.answerBuffer.Len() > 0 {
		entry(m.agentStyle.Render("Agent: ") + m.answerBuffer.String())
	}
	return b.String()
}
