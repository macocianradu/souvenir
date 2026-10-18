package ui

import (
	"git.estatecloud.org/radumaco/souvenir/model"
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
	}

	messages := m.conversation.Messages
	activityAt := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			activityAt = i + 1
			break
		}
	}

	for i, message := range messages {
		if i == activityAt {
			activity()
		}
		if message.IsToolRound() {
			b.WriteString(toolStyle.Width(width).Render(toolLine(message)))
			b.WriteString("\n")
			if i+1 == len(messages) || !messages[i+1].IsToolRound() {
				b.WriteString("\n")
			}
			continue
		}
		switch message.Role {
		case "user":
			entry(lipgloss.PlaceHorizontal(width, lipgloss.Right, m.senderStyle.Render("You: ")+userText(message)))
		case "assistant":
			entry(m.agentStyle.Render("Agent: ") + formatMarkdown(message.Content))
		}
	}

	if activityAt == len(messages) {
		activity()
	}
	if m.answerBuffer.Len() > 0 {
		entry(m.agentStyle.Render("Agent: ") + formatMarkdown(m.answerBuffer.String()))
	}
	return b.String()
}

func toolLine(msg model.Message) string {
	if msg.Role == "tool" {
		return "↳ " + truncate(msg.Content, 100)
	}
	line := truncate(msg.Content, 200)
	for _, call := range msg.ToolCalls {
		line += "\n⚙ " + call.Function.Name + " " + truncate(call.Function.Arguments, 80)
	}
	return strings.TrimPrefix(line, "\n")
}

func userText(message model.Message) string {
	if len(message.Images) == 0 {
		return message.Content
	}
	return strings.TrimSpace(message.Content + " " + toolStyle.Render(imageLabel(len(message.Images))))
}
