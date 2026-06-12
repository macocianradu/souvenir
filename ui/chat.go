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

var logger *slog.Logger

type uiModel struct {
	viewport    viewport.Model
	messages    []model.Message
	textarea    textarea.Model
	senderStyle lipgloss.Style
	agentStyle  lipgloss.Style
	errorStyle  lipgloss.Style
	client      llm.LLMClient
	width		int
	height		int
	waiting     bool
	err         error
}

type agentResponseMessage struct {
	response []model.Message
	err      error
}

func InitialModel(config config.Config) uiModel {
	logger = slog.Default().With("Component", "UI")
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

	return uiModel{
		textarea:    ta,
		messages:    []model.Message{},
		viewport:    vp,
		senderStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		errorStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		agentStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("86")),
		client:      llm.LLMClient{Cfg: config},
		err:         nil,
	}
}

func (m uiModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		logger.Debug("Received WindowSizeMsg", "width", msg.Width, "height", msg.Height)
		m.viewport.SetWidth(msg.Width * 80 / 100)
		m.textarea.SetWidth(msg.Width * 80 / 100)
		m.viewport.SetHeight(msg.Height - lipgloss.Height(m.textarea.View()))
		m.width = msg.Width
		m.height = msg.Height

		var messages = m.renderMessages()

		if len(m.messages) > 0 {
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		} else {
			m.viewport.SetContent(renderLanding(m.viewport.Width(), m.viewport.Height()))
		}
		logger.Debug("New sizes",
			"vp width", m.viewport.Width(), "vp height", m.viewport.Height(),
			"ta width", m.textarea.Width(), "ta height", m.textarea.Height())
		m.viewport.GotoBottom()

	case tea.KeyPressMsg:
		logger.Debug("Received KeyPressMsg", "msg", msg)
		switch msg.String() {
		case "ctrl+c", "esc":
			logger.Debug("Received exit sequence. Quiting")
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
			logger.Debug("Received enter. Creating new message", "message", input)
			var messages = m.renderMessages()
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
			m.textarea.Reset()
			m.viewport.GotoBottom()
			m.waiting = true
			return m, m.callAgent(input, m.messages)
		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		}

	case cursor.BlinkMsg:
		logger.Debug("Received BlinkMsg")
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case agentResponseMessage:
		logger.Debug("Received agentResponseMessage")
		m.waiting = false
		resp := msg.response
		if msg.err != nil {
			logger.Error("Message is error", "error", msg.err)
			resp = []model.Message{{
				Role:    "Error",
				Content: msg.err.Error(),
			}}
		}

		for _, r := range resp {
			logger.Debug("Appending response message", "message", r)
			m.messages = append(m.messages, r)
		}
		var messages = m.renderMessages()
		m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		m.viewport.GotoBottom()
		return m, nil
	}

	return m, nil
}

func (m uiModel) View() tea.View {
	ui := lipgloss.JoinVertical(
		lipgloss.Left,
		m.viewport.View(),
		m.textarea.View())
	c := m.textarea.Cursor()
	if c != nil {
		c.Y += lipgloss.Height(m.viewport.View())
		gap := m.width - lipgloss.Width(ui)
		c.X += max(gap / 2, 0)
	}
	view := tea.NewView(lipgloss.PlaceHorizontal(m.width, lipgloss.Center, ui))
	view.Cursor = c
	view.AltScreen = true
	return view
}

func (m uiModel) callAgent(input string, messages []model.Message) tea.Cmd {
	logger.Debug("Calling agent", "query", input, "messages", messages)
	return func() tea.Msg {
		resp, err := m.client.Call(input, messages)
		return agentResponseMessage{response: resp, err: err}
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
		case "error":
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
