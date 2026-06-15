package ui

import (
	"fmt"
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

type focusState int

const (
	focusChat focusState = iota
	focusOverlay
)

type uiModel struct {
	logger      slog.Logger
	viewport    viewport.Model
	messages    []model.Message
	textarea    textarea.Model
	focus       focusState
	picker      modelPicker
	senderStyle lipgloss.Style
	agentStyle  lipgloss.Style
	errorStyle  lipgloss.Style
	client      llm.LLMClient
	width       int
	height      int
	waiting     bool
	err         error
}

type agentResponseMessage struct {
	response []model.Message
	err      error
}

type modelsResponseMessage struct {
	models []string
	err    error
}

func InitialModel(config config.Config) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.SetVirtualCursor(false)
	ta.Focus()

	ta.Prompt = "⦀ "
	ta.CharLimit = 280

	ta.SetWidth(30)
	ta.SetHeight(3)

	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)

	ta.ShowLineNumbers = false

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))
	vp.SetContent(renderLanding(vp.Width(), vp.Height()))
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	ta.KeyMap.InsertNewline.SetEnabled(false)

	model := uiModel{
		textarea:    ta,
		messages:    []model.Message{},
		viewport:    vp,
		senderStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		errorStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		agentStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("86")),
		client:      llm.LLMClient{Cfg: config, Logger: *slog.Default().With("Component", "LLM")},
		focus:       focusChat,
		logger:      *slog.Default().With("Component", "TUI"),
		err:         nil,
	}
	model.picker = newModelPicker(&model.focus)
	return model
}

func (m uiModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.logger.Debug("Received message", "msg", msg)

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.SetWidth(msg.Width * 80 / 100)
		m.textarea.SetWidth(msg.Width * 80 / 100)
		m.viewport.SetHeight(msg.Height - lipgloss.Height(m.textarea.View()) - 1)
		m.width = msg.Width
		m.height = msg.Height
		m.picker.SetSize(msg.Width, msg.Height)

		var messages = m.renderMessages()

		if len(m.messages) > 0 {
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		} else {
			m.viewport.SetContent(renderLanding(m.viewport.Width(), m.viewport.Height()))
		}
		m.logger.Debug("New sizes",
			"vp width", m.viewport.Width(), "vp height", m.viewport.Height(),
			"ta width", m.textarea.Width(), "ta height", m.textarea.Height())
		m.viewport.GotoBottom()

	case tea.KeyPressMsg:
		if m.focus == focusOverlay {
			picker, cmd := m.picker.Update(msg)
			m.picker = picker
			return m, cmd
		}
		switch msg.String() {
		case "esc":
			if m.focus == focusOverlay {
				m.focus = focusChat
			}
			return m, nil
		case "ctrl+c":
			fmt.Println(m.textarea.Value())
			return m, tea.Quit
		case "shift+enter", "ctrl+j", "alt+enter":
			m.textarea.InsertRune('\n')
			return m, nil
		case "enter":
			input := m.textarea.Value()
			m.messages = append(m.messages, model.Message{
				Content: input,
				Role:    "user",
			})
			m.logger.Debug("Received enter. Creating new message", "message", input)
			switch input {
			case "/models":
				m.logger.Debug("Received models sequence")
				m.focus = focusOverlay
				return m, m.getModels()
			case "/exit":
				m.logger.Debug("Received exit sequence. Quiting")
				fmt.Println(m.textarea.Value())
				return m, tea.Quit
			default:
				var messages = m.renderMessages()
				m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
				m.textarea.Reset()
				m.viewport.GotoBottom()
				m.waiting = true
				return m, m.callAgent(input, m.messages)
			}
		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		}

	case cursor.BlinkMsg:
		m.logger.Debug("Received BlinkMsg")
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case agentResponseMessage:
		m.logger.Debug("Received agentResponseMessage")
		m.waiting = false
		resp := msg.response
		if msg.err != nil {
			m.logger.Error("Message is error", "error", msg.err)
			resp = []model.Message{{
				Role:    "Error",
				Content: msg.err.Error(),
			}}
		}

		for _, r := range resp {
			m.logger.Debug("Appending response message", "message", r)
			m.messages = append(m.messages, r)
		}
		var messages = m.renderMessages()
		m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		m.viewport.GotoBottom()
		return m, nil

	case modelChosenMsg:
		m.logger.Debug("Received modelChosenMsg", "model", msg.id)
		m.client.Cfg.Llm.Model = msg.id
		m.waiting = false
		m.focus = focusChat
	}

	if m.focus == focusOverlay {
		picker, cmd := m.picker.Update(msg)
		m.picker = picker
		return m, cmd
	}

	return m, nil
}

func (m uiModel) View() tea.View {
	if m.focus == focusOverlay {
		return tea.NewView(lipgloss.Place(m.width, m.height,
			lipgloss.Center, lipgloss.Center,
			m.picker.View()))
	}
	ui := lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		"model: " + m.client.Cfg.Llm.Model + "; url: " + m.client.Cfg.Api.Url,
		m.textarea.View())
	c := m.textarea.Cursor()
	if c != nil {
		c.Y += lipgloss.Height(m.viewport.View()) + 1
		gap := m.width - lipgloss.Width(ui)
		c.X += max(gap/2, 0)
	}
	view := tea.NewView(lipgloss.PlaceHorizontal(m.width, lipgloss.Center, ui))
	view.Cursor = c
	view.AltScreen = true
	return view
}

func (m uiModel) callAgent(input string, messages []model.Message) tea.Cmd {
	m.logger.Debug("Calling agent", "query", input, "messages", messages)
	return func() tea.Msg {
		resp, err := m.client.Call(input, messages)
		return agentResponseMessage{response: resp, err: err}
	}
}

func (m uiModel) getModels() tea.Cmd {
	m.logger.Debug("Querying models")
	return func() tea.Msg {
		resp, _ := m.client.Models()
		m.logger.Debug("Received models from llm", "models", resp)
		return modelsLoadedMsg{models: resp}
	}
}

func (m uiModel) renderMessages() string {
	var result strings.Builder
	var width = m.viewport.Width()
	for _, message := range m.messages {
		switch message.Role {
		case "user":
			var line strings.Builder
			line.WriteString(m.senderStyle.Render("You: "))
			line.WriteString(message.Content)
			result.WriteString(lipgloss.PlaceHorizontal(width, lipgloss.Right, line.String()))
		case "assistant":
			result.WriteString(m.agentStyle.Render("Agent: "))
			result.WriteString(message.Content)
		case "Error":
			result.WriteString(m.errorStyle.Render("Error:"))
			result.WriteString(message.Content)
		}
		result.WriteString("\n")

		var sepStyle lipgloss.Style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			MarginTop(1).
			MarginBottom(1)
		sep := sepStyle.Render(strings.Repeat("┈", width))
		result.WriteString(sep)
		result.WriteString("\n")
	}
	return result.String()
}
