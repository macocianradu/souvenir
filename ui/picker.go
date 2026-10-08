package ui

import (
	"log/slog"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	deleteKey    = key.NewBinding(key.WithKeys("x", "delete"), key.WithHelp("x", "delete"))
	confirmStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Padding(0, 2)
)

type picker struct {
	logger    slog.Logger
	list      list.Model
	title     string
	height    int
	deletable bool
	confirm   *modelItem
}

type modelsLoadedMsg struct {
	models    []modelItem
	title     string
	err       error
	deletable bool
}
type pickerChosenMsg struct{ id string }
type pickerDismissMsg struct{}
type pickerDeleteMsg struct{ item modelItem }

type modelItem struct {
	id          string
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
	p.height = h
	p.list.SetSize(w, h)
}

func (p picker) Update(msg tea.Msg) (picker, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		p.logger.Debug("Received modelsLoadedMsg", "models", len(msg.models))
		p.list.Title = msg.title
		p.deletable = msg.deletable
		p.confirm = nil
		deletable := msg.deletable
		p.list.AdditionalShortHelpKeys = func() []key.Binding {
			if deletable {
				return []key.Binding{deleteKey}
			}
			return nil
		}
		items := make([]list.Item, len(msg.models))
		for i, m := range msg.models {
			items[i] = m
		}
		return p, p.list.SetItems(items)
	case tea.KeyPressMsg:
		if p.confirm != nil {
			item := *p.confirm
			p.setConfirm(nil)
			if msg.String() == "y" {
				return p, func() tea.Msg { return pickerDeleteMsg{item} }
			}
			return p, nil
		}
		filtering := p.list.FilterState() == list.Filtering
		if !filtering && (msg.String() == "q" || msg.String() == "esc") {
			return p, func() tea.Msg { return pickerDismissMsg{} }
		}
		if !filtering && p.deletable && key.Matches(msg, deleteKey) {
			if it, ok := p.list.SelectedItem().(modelItem); ok {
				p.setConfirm(&it)
			}
			return p, nil
		}
		if msg.String() == "enter" {
			if it, ok := p.list.SelectedItem().(modelItem); ok {
				return p, func() tea.Msg { return pickerChosenMsg{it.id} }
			}
		}
	}
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}

func (p *picker) setConfirm(item *modelItem) {
	p.confirm = item
	if item != nil {
		p.list.SetHeight(max(p.height-lipgloss.Height(p.confirmView()), 1))
	} else {
		p.list.SetHeight(p.height)
	}
}

func (p *picker) remove(id string) {
	for i, it := range p.list.Items() {
		if it.(modelItem).id == id {
			p.list.RemoveItem(i)
			return
		}
	}
}

func (p picker) confirmView() string {
	return confirmStyle.Render("Delete \"" + p.confirm.name + "\" permanently? Its messages, summaries " +
		"and search index are removed and cannot be recovered. y to delete, any other key to cancel.")
}

func (p picker) View() string {
	if p.confirm != nil {
		return lipgloss.JoinVertical(lipgloss.Left, p.list.View(), p.confirmView())
	}
	return p.list.View()
}
