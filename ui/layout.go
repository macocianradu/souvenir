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
	m.picker.SetSize(width, height)
	m.commands.list.SetSize(contentWidth-dropdownStyle.GetHorizontalFrameSize(), commandDropdownHeight)
	m.layout()
	m.logger.Debug("New sizes",
		"vp width", m.viewport.Width(), "vp height", m.viewport.Height(),
		"ta width", m.textarea.Width(), "ta height", m.textarea.Height())

	m.refreshViewport()
}

func (m *uiModel) refreshViewport() {
	follow := m.viewport.AtBottom()
	if len(m.conversation.Messages) == 0 && m.answerBuffer.Len() == 0 {
		m.viewport.SetContent(renderLanding(m.viewport.Width(), m.viewport.Height()))
	} else {
		m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(m.renderMessages()))
	}
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *uiModel) layout() {
	h := m.height - lipgloss.Height(m.textarea.View())
	if m.statusMessage != "" {
		h -= lipgloss.Height(m.statusMessage)
	}
	if m.pending > 0 {
		h -= lipgloss.Height(m.spinner.View())
	}
	if m.commands.open {
		h -= lipgloss.Height(m.commands.view())
	}
	follow := m.viewport.AtBottom()
	m.viewport.SetHeight(max(h, 1))
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *uiModel) startWait() {
	m.pending++
	m.layout()
}

func (m *uiModel) stopWait() {
	m.pending = max(m.pending-1, 0)
	m.layout()
}

func (m *uiModel) setErrorMessage(message string) {
	m.statusMessage = m.errorStyle.Render(message)
	m.layout()
}

func (m *uiModel) setStatusMessage(message string) {
	if message == "" {
		message = "model: " + m.client.Cfg.Llm.Model + " url: " + m.client.Cfg.Api.Url
	}
	m.statusMessage = m.statusStyle.Render(message)
	m.layout()
}

func (m *uiModel) openCommands() {
	m.commands.open = true
	name, _, _ := strings.Cut(strings.TrimPrefix(m.textarea.Value(), "/"), " ")
	m.commands.filter(name)
	m.layout()
}

func (m *uiModel) closeCommands() {
	if m.commands.open {
		m.commands.open = false
		m.layout()
	}
}
