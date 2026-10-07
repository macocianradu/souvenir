package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

func (m *uiModel) resize(width, height int) {
	m.width = width
	m.height = height
	contentWidth := width * 80 / 100

	m.viewport.SetWidth(contentWidth)
	m.textarea.SetWidth(contentWidth)
	m.viewport.SetHeight(height - lipgloss.Height(m.textarea.View()) - 1)
	m.picker.SetSize(width, height)
	m.commands.list.SetSize(contentWidth, commandDropdownHeight)
	if m.commands.open {
		m.viewport.SetHeight(m.viewport.Height() - commandDropdownHeight)
	}
	m.logger.Debug("New sizes",
		"vp width", m.viewport.Width(), "vp height", m.viewport.Height(),
		"ta width", m.textarea.Width(), "ta height", m.textarea.Height())

	m.refreshViewport()
}

// refreshViewport re-renders the conversation, or the landing screen when
// there is nothing to show yet, and scrolls to the bottom.
func (m *uiModel) refreshViewport() {
	if len(m.conversation.Messages) == 0 && m.answerBuffer.Len() == 0 {
		m.viewport.SetContent(renderLanding(m.viewport.Width(), m.viewport.Height()))
	} else {
		m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(m.renderMessages()))
	}
	m.viewport.GotoBottom()
}

func (m *uiModel) startWait() {
	if !m.waiting {
		m.viewport.SetHeight(m.viewport.Height() - 1)
		m.waiting = true
	}
}

func (m *uiModel) stopWait() {
	if m.waiting {
		m.waiting = false
		m.viewport.SetHeight(m.viewport.Height() + 1)
	}
}

func (m *uiModel) setErrorMessage(message string) {
	m.statusMessage = m.errorStyle.Render(message)
}

func (m *uiModel) setStatusMessage(message string) {
	if message == "" {
		m.statusMessage = m.statusStyle.Render("model: " + m.client.Cfg.Llm.Model + " url: " + m.client.Cfg.Api.Url)
		return
	}
	m.statusMessage = m.statusStyle.Render(message)
}

func (m *uiModel) openCommands() {
	if !m.commands.open {
		m.commands.open = true
		m.viewport.SetHeight(m.viewport.Height() - commandDropdownHeight)
	}
	m.commands.filter(strings.TrimPrefix(m.textarea.Value(), "/"))
}

func (m *uiModel) closeCommands() {
	if m.commands.open {
		m.commands.open = false
		m.viewport.SetHeight(m.viewport.Height() + commandDropdownHeight)
	}
}
