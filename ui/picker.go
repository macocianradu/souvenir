package ui

import (
	"log/slog"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

type picker struct {
	logger slog.Logger
	list   list.Model
	title  string
}

type modelsLoadedMsg struct {
	models []modelItem
	title  string
}
type pickerChosenMsg struct{ id string }
type pickerDismissMsg struct {}

type modelItem struct {
	name        string
	description string
}

func (i modelItem) FilterValue() string { return i.name }
func (i modelItem) Title() string       { return i.name }
func (i modelItem) Description() string { return i.description }

func newPicker() picker {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.DisableQuitKeybindings()
	return picker{list: l, logger: *slog.Default().With("Component", "Picker")}
}

func (p *picker) SetSize(w, h int) {
	p.list.SetSize(w, h)
}

func (p picker) Update(msg tea.Msg) (picker, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		p.logger.Debug("Received modelsLoadedMsg", "models", msg.models)
		p.list.Title = msg.title
		items := make([]list.Item, len(msg.models))
		for i, m := range msg.models {
			items[i] = modelItem{name: m.name, description: m.description}
		}
		return p, p.list.SetItems(items)
	case tea.KeyPressMsg:
		if msg.String() == "q" || msg.String() == "esc" {
			if p.list.FilterState() != list.Filtering {
				return p, func() tea.Msg { return pickerDismissMsg{} }
			}
		}
		if msg.String() == "enter" {
			p.logger.Debug("Selecting model", "model", p.list.SelectedItem())
			if it, ok := p.list.SelectedItem().(modelItem); ok {
				p.logger.Debug("Sending modelChosenMsg")
				return p, func() tea.Msg { return pickerChosenMsg{it.name} }
			}
		}
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}

func (p picker) View() string {
	return p.list.View()
}
