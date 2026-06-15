package ui

import (
	"log/slog"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
)

type modelPicker struct {
	focus *focusState
	logger slog.Logger
	list list.Model
}

type modelsLoadedMsg struct{ models []string }
type modelsErrMsg    struct{ err error }
type modelChosenMsg  struct{ id string }

type modelItem struct{ name string }
func (i modelItem) FilterValue() string { return i.name }
func (i modelItem) Title() string       { return i.name }
func (i modelItem) Description() string { return i.name }

func newModelPicker(focus *focusState) modelPicker {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.Title = "Choose a model"
	return modelPicker{list: l, logger: *slog.Default().With("Component", "ModelPicker"), focus: focus}
}

func (p *modelPicker) SetSize(w, h int) {
	p.list.SetSize(w, h)
}

func (p modelPicker) Update(msg tea.Msg) (modelPicker, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		p.logger.Debug("Received modelsLoadedMsg", "models", msg.models)
		items := make([]list.Item, len(msg.models))
		for i, m := range msg.models {
			items[i] = modelItem{ name: m}
		}
		return p, p.list.SetItems(items)
	case tea.KeyPressMsg:
		if msg.String() == "enter" {
			p.logger.Debug("Selecting model", "model", p.list.SelectedItem())
			if it, ok := p.list.SelectedItem().(modelItem); ok {
				*p.focus = focusChat
				p.logger.Debug("Sending modelChosenMsg")
				return p, func() tea.Msg { return modelChosenMsg{it.name} }
			}
		}
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}

func (p modelPicker) View() string {
	return p.list.View()
}
