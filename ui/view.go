package ui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func (m uiModel) View() tea.View {
	if m.focus != focusChat {
		return tea.NewView(lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.picker.View()))
	}

	parts := []string{m.viewport.View()}
	if m.statusMessage != "" {
		parts = append(parts, m.statusMessage)
	}
	if m.waiting {
		parts = append(parts, m.spinner.View())
	}
	if m.commands.open {
		parts = append(parts, m.commands.view())
	}
	above := lipgloss.JoinVertical(lipgloss.Left, parts...)
	ui := lipgloss.JoinVertical(lipgloss.Left, above, m.textarea.View())

	c := m.textarea.Cursor()
	if c != nil {
		c.Y += lipgloss.Height(above)
		c.X += max((m.width-lipgloss.Width(ui))/2, 0)
	}
	view := tea.NewView(lipgloss.PlaceHorizontal(m.width, lipgloss.Center, ui))
	view.Cursor = c
	view.AltScreen = true
	return view
}
