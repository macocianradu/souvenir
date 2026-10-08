package ui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/sahilm/fuzzy"
)

const commandDropdownHeight = 8

var (
	dropdownStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Padding(0, 1)

	inlineTitleStyle    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("86")).Bold(true)
	inlineTitleSelStyle = lipgloss.NewStyle().Background(lipgloss.Color("60")).Foreground(lipgloss.Color("86")).Bold(true)
	inlineDescStyle     = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("245"))
	inlineDescSelStyle  = lipgloss.NewStyle().Background(lipgloss.Color("60")).Foreground(lipgloss.Color("255"))
	inlinePadStyle      = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	inlinePadSelStyle   = lipgloss.NewStyle().Background(lipgloss.Color("60"))
)

type inlineDelegate struct{}

func (inlineDelegate) Height() int                             { return 1 }
func (inlineDelegate) Spacing() int                            { return 0 }
func (inlineDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (inlineDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	c, ok := item.(command)
	if !ok {
		return
	}
	if m.Width() <= 0 {
		return
	}
	title := "/" + c.name
	desc := c.description

	titleStyle := inlineTitleStyle
	descStyle := inlineDescStyle
	padStyle := inlinePadStyle
	if index == m.Index() {
		titleStyle = inlineTitleSelStyle
		descStyle = inlineDescSelStyle
		padStyle = inlinePadSelStyle
	}

	renderedTitle := titleStyle.Render(title)
	renderedDesc := descStyle.Render(desc)
	pad := max(m.Width()-1-lipgloss.Width(renderedTitle)-lipgloss.Width(renderedDesc), 1)
	fmt.Fprintf(w, "%s%s%s%s", renderedTitle, padStyle.Render(" "), renderedDesc, padStyle.Render(strings.Repeat(" ", pad)))
}

type command struct {
	name        string
	description string
	handler     func(m uiModel, args string) (uiModel, tea.Cmd)
}

func (c command) FilterValue() string { return c.name }
func (c command) Title() string       { return "/" + c.name }
func (c command) Description() string { return c.description }

type commandList struct {
	list      list.Model
	open      bool
	available []command
}

func newCommandList(width int) commandList {
	l := list.New(nil, inlineDelegate{}, width, 1)
	l.Title = "Commands"
	l.SetShowFilter(false)
	l.SetFilteringEnabled(false)
	l.SetShowStatusBar(false)
	l.SetShowPagination(false)
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	return commandList{list: l}
}

func (m uiModel) buildCommands() []command {
	return []command{
		{
			name:        "exit",
			description: "Quit the application",
			handler: func(m uiModel, _ string) (uiModel, tea.Cmd) {
				m.logger.Debug("Received exit sequence. Quiting")
				return m, tea.Quit
			},
		},
		{
			name:        "history",
			description: "Open conversation history",
			handler: func(m uiModel, _ string) (uiModel, tea.Cmd) {
				m.logger.Debug("Received history call")
				if !m.canSwitchConversation() {
					return m, nil
				}
				m.focus = focusHistory
				return m, m.getHistory()
			},
		},
		{
			name:        "new",
			description: "Start a new conversation",
			handler: func(m uiModel, _ string) (uiModel, tea.Cmd) {
				m.logger.Debug("Received new conversation request")
				if !m.canSwitchConversation() {
					return m, nil
				}
				m.openConversation(model.Conversation{})
				m.setStatusMessage("Started a new conversation")
				return m, nil
			},
		},
		{
			name:        "models",
			description: "Choose an LLM model",
			handler: func(m uiModel, _ string) (uiModel, tea.Cmd) {
				m.logger.Debug("Received models sequence")
				m.focus = focusModels
				return m, m.getModels()
			},
		},
		{
			name:        "rename",
			description: "Generate a title, or /rename <title> to set one",
			handler: func(m uiModel, args string) (uiModel, tea.Cmd) {
				m.logger.Debug("Received rename request", "title", args)
				if args != "" {
					m.conversation.Title = args
					m.conversation.TitleSource = model.TitleSourceUser
					m.setStatusMessage("Conversation renamed to: " + args)
					return m, m.requestSave()
				}
				if len(m.conversation.Messages) == 0 {
					m.setErrorMessage("Nothing to rename yet")
					return m, nil
				}
				m.startWait()
				return m, tea.Batch(m.renameConversation(false), m.spinner.Tick)
			},
		},
	}
}

func (m *uiModel) canSwitchConversation() bool {
	if m.streaming || m.saving || m.pending > 0 {
		m.setErrorMessage("Wait for the current request to finish before switching conversations")
		return false
	}
	return true
}

func (cl *commandList) setAvailable(cmds []command) {
	cl.available = cmds
	items := make([]list.Item, len(cmds))
	for i, c := range cmds {
		items[i] = c
	}
	cl.list.SetItems(items)
	cl.list.SetHeight(len(items) + 1)
}

func (cl *commandList) filter(query string) {
	var matches []list.Item
	if query == "" {
		matches = make([]list.Item, len(cl.available))
		for i, c := range cl.available {
			matches[i] = c
		}
	} else {
		names := make([]string, len(cl.available))
		for i, c := range cl.available {
			names[i] = c.name
		}
		result := fuzzy.Find(query, names)
		matches = make([]list.Item, len(result))
		for i, r := range result {
			matches[i] = cl.available[r.Index]
		}
	}
	cl.list.SetItems(matches)
	cl.list.ResetSelected()
}

func (cl commandList) view() string {
	return dropdownStyle.Render(cl.list.View())
}

func (cl commandList) selectedItem() (command, bool) {
	c, ok := cl.list.SelectedItem().(command)
	return c, ok
}
