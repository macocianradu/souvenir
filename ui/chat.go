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
)

var logger *slog.Logger

type model struct {
	viewport      viewport.Model
	messages      []llm.Message
	textarea      textarea.Model
	senderStyle   lipgloss.Style
	receiverStyle lipgloss.Style
	client        llm.LLMClient
	waiting       bool
	err           error
}

type agentResponseMessage struct {
	response []llm.Message
	err      error
}

func InitialModel(config config.Config) model {
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
	vp.SetContent(`What are we doing on today?`)
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	ta.KeyMap.InsertNewline.SetEnabled(false)

	return model{
		textarea:    ta,
		messages:    []llm.Message{},
		viewport:    vp,
		senderStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		client:      llm.LLMClient{Cfg: config},
		err:         nil,
	}
}

func (m model) Init() tea.Cmd {
	return textarea.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		logger.Debug("Received WindowSizeMsg", "width", msg.Width, "height", msg.Height)
		m.viewport.SetWidth(msg.Width)
		m.textarea.SetWidth(msg.Width)
		m.viewport.SetHeight(msg.Height - m.textarea.Height())

		var messages = getMessages(m.messages)

		if len(m.messages) > 0 {
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		}
		m.viewport.GotoBottom()

	case tea.KeyPressMsg:
		logger.Debug("Received KeyPressMsg")
		switch msg.String() {
		case "ctrl+c", "esc":
			logger.Debug("Received exit sequence. Quiting")
			fmt.Println(m.textarea.Value())
			return m, tea.Quit
		case "enter":
			input := m.textarea.Value()
			m.messages = append(m.messages, llm.Message{
				Content: m.senderStyle.Render(input),
				Role:    "You",
			})
			logger.Debug("Received enter. Creating new message", "message", input)
			var messages = getMessages(m.messages)
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
			resp = []llm.Message{{
				Role:    "Error",
				Content: msg.err.Error(),
			}}
		}

		for _, r := range resp {
			logger.Debug("Appending response message", "message", r)
			m.messages = append(m.messages, r)
		}
		var messages = getMessages(m.messages)
		m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		return m, nil
	}

	return m, nil
}

func (m model) View() tea.View {
	viewportView := m.viewport.View()
	v := tea.NewView(viewportView + "\n" + m.textarea.View())
	c := m.textarea.Cursor()
	if c != nil {
		c.Y += lipgloss.Height(viewportView)
	}
	v.Cursor = c
	v.AltScreen = true
	return v
}

func (m model) callAgent(input string, messages []llm.Message) tea.Cmd {
	logger.Debug("Calling agent", "query", input, "messages", messages)
	return func() tea.Msg {
		resp, err := m.client.Call(input, messages)
		return agentResponseMessage{response: resp, err: err}
	}
}

func getMessages(m []llm.Message) string {
	var messages strings.Builder
	for _, m := range m {
		messages.WriteString(m.Role)
		messages.WriteString(": ")
		messages.WriteString(m.Content)
		messages.WriteString("\n")
	}
	return messages.String()
}
