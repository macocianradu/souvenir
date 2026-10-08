package ui

import (
	"log/slog"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	deleteKey    = key.NewBinding(key.WithKeys("x", "delete"), key.WithHelp("x", "delete"))
	searchKey    = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search"))
	confirmStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Padding(0, 2)
	queryStyle   = lipgloss.NewStyle().Padding(0, 2)
)

type picker struct {
	logger     slog.Logger
	list       list.Model
	query      textinput.Model
	width      int
	height     int
	searchable bool
	allItems   []list.Item
	confirm    *modelItem
}

type modelsLoadedMsg struct {
	models     []modelItem
	title      string
	err        error
	searchable bool
}
type pickerChosenMsg struct{ id string }
type pickerDismissMsg struct{}
type pickerDeleteMsg struct{ item modelItem }
type pickerSearchMsg struct{ query string }
type pickerResultsMsg struct {
	query string
	items []modelItem
	err   error
}

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
	q := textinput.New()
	q.Prompt = "Search: "
	q.Placeholder = "words or a description, Enter to search"
	return picker{list: l, query: q, logger: *slog.Default().With("Component", "Picker")}
}

func (p *picker) SetSize(w, h int) {
	p.width, p.height = w, h
	p.query.SetWidth(w - queryStyle.GetHorizontalFrameSize() - lipgloss.Width(p.query.Prompt) - 1)
	p.layout()
}

func (p picker) Update(msg tea.Msg) (picker, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		p.logger.Debug("Received modelsLoadedMsg", "models", len(msg.models))
		p.list.Title = msg.title
		p.searchable = msg.searchable
		p.list.SetFilteringEnabled(!msg.searchable)
		searchable := msg.searchable
		p.list.AdditionalShortHelpKeys = func() []key.Binding {
			if searchable {
				return []key.Binding{searchKey, deleteKey}
			}
			return nil
		}
		p.confirm = nil
		p.query.Reset()
		p.query.Blur()
		p.allItems = toItems(msg.models)
		p.layout()
		return p, p.list.SetItems(p.allItems)

	case pickerResultsMsg:
		if msg.query != p.query.Value() {
			return p, nil
		}
		if msg.err != nil {
			return p, p.list.NewStatusMessage("Search failed: " + msg.err.Error())
		}
		cmd := p.list.SetItems(toItems(msg.items))
		p.list.ResetSelected()
		if len(msg.items) == 0 {
			return p, tea.Batch(cmd, p.list.NewStatusMessage("No matches"))
		}
		return p, cmd

	case tea.KeyPressMsg:
		if p.confirm != nil {
			item := *p.confirm
			p.confirm = nil
			p.layout()
			if msg.String() == "y" {
				return p, func() tea.Msg { return pickerDeleteMsg{item} }
			}
			return p, nil
		}
		if p.query.Focused() {
			return p.updateQuery(msg)
		}
		filtering := p.list.FilterState() == list.Filtering
		switch {
		case filtering:
		case msg.String() == "esc" && p.query.Value() != "":
			p.query.Reset()
			p.layout()
			return p, p.list.SetItems(p.allItems)
		case msg.String() == "q" || msg.String() == "esc":
			return p, func() tea.Msg { return pickerDismissMsg{} }
		case p.searchable && key.Matches(msg, searchKey):
			cmd := p.query.Focus()
			p.layout()
			return p, cmd
		case p.searchable && key.Matches(msg, deleteKey):
			if it, ok := p.list.SelectedItem().(modelItem); ok {
				p.confirm = &it
				p.layout()
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
	if p.query.Focused() {
		var qcmd tea.Cmd
		p.query, qcmd = p.query.Update(msg)
		cmd = tea.Batch(cmd, qcmd)
	}
	return p, cmd
}

func (p picker) updateQuery(msg tea.KeyPressMsg) (picker, tea.Cmd) {
	switch msg.String() {
	case "esc":
		p.query.Blur()
		p.layout()
		return p, nil
	case "enter":
		p.query.Blur()
		p.layout()
		query := p.query.Value()
		if query == "" {
			return p, p.list.SetItems(p.allItems)
		}
		return p, func() tea.Msg { return pickerSearchMsg{query} }
	}
	var cmd tea.Cmd
	p.query, cmd = p.query.Update(msg)
	return p, cmd
}

func (p *picker) layout() {
	h := p.height
	if p.showQuery() {
		h -= lipgloss.Height(p.queryView())
	}
	if p.confirm != nil {
		h -= lipgloss.Height(p.confirmView())
	}
	p.list.SetSize(p.width, max(h, 1))
}

func (p picker) showQuery() bool {
	return p.query.Focused() || p.query.Value() != ""
}

func (p *picker) remove(id string) {
	for i, it := range p.list.Items() {
		if it.(modelItem).id == id {
			p.list.RemoveItem(i)
			break
		}
	}
	for i, it := range p.allItems {
		if it.(modelItem).id == id {
			p.allItems = append(p.allItems[:i:i], p.allItems[i+1:]...)
			break
		}
	}
}

func (p picker) queryView() string {
	return queryStyle.Render(p.query.View())
}

func (p picker) confirmView() string {
	return confirmStyle.Render("Delete \"" + p.confirm.name + "\" permanently? Its messages, summaries " +
		"and search index are removed and cannot be recovered. y to delete, any other key to cancel.")
}

func (p picker) View() string {
	parts := []string{}
	if p.showQuery() {
		parts = append(parts, p.queryView())
	}
	parts = append(parts, p.list.View())
	if p.confirm != nil {
		parts = append(parts, p.confirmView())
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func toItems(models []modelItem) []list.Item {
	items := make([]list.Item, len(models))
	for i, m := range models {
		items[i] = m
	}
	return items
}
