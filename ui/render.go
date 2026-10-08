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

var toolStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("245"))

func truncate(s string, n int) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n-1]) + "…"
}

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

	activity := func() {
		for _, mem := range m.turnMemories {
			b.WriteString(toolStyle.Width(width).Render("◆ " + truncate(mem.Content, 100)))
			b.WriteString("\n")
		}
		if m.thinkingBuffer.Len() > 0 {
			thinking()
		}
		for _, msg := range m.toolTrail {
			line := "↳ " + truncate(msg.Content, 100)
			if msg.Role != "tool" {
				line = truncate(msg.Content, 200)
				for _, call := range msg.ToolCalls {
					line += "\n⚙ " + call.Function.Name + " " + truncate(call.Function.Arguments, 80)
				}
				line = strings.TrimPrefix(line, "\n")
			}
			b.WriteString(toolStyle.Width(width).Render(line))
			b.WriteString("\n")
		}
		if len(m.toolTrail) > 0 {
			b.WriteString("\n")
		}
	}

	messages := m.conversation.Messages
	activityAt := len(messages)
	if last := len(messages) - 1; !m.streaming && last >= 0 && messages[last].Role == "assistant" {
		activityAt = last
	}

	for i, message := range messages {
		if i == activityAt {
			activity()
		}
		switch message.Role {
		case "user":
			entry(lipgloss.PlaceHorizontal(width, lipgloss.Right, m.senderStyle.Render("You: ")+message.Content))
		case "assistant":
			entry(m.agentStyle.Render("Agent: ") + message.Content)
		}
	}

	if activityAt == len(messages) {
		activity()
	}
	if m.answerBuffer.Len() > 0 {
		entry(m.agentStyle.Render("Agent: ") + m.answerBuffer.String())
	}
	return b.String()
}
