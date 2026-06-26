package ui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

type focusState int

const (
	focusChat focusState = iota
	focusModels
	focusHistory
)

type uiModel struct {
	logger         slog.Logger
	viewport       viewport.Model
	spinner        spinner.Model
	conversation   model.Conversation
	textarea       textarea.Model
	focus          focusState
	picker         picker
	thinkingBuffer strings.Builder
	answerBuffer   strings.Builder
	senderStyle    lipgloss.Style
	agentStyle     lipgloss.Style
	errorStyle     lipgloss.Style
	statusStyle    lipgloss.Style
	client         llm.ChatClient
	statusMessage  string
	commands       commandList
	history        history.DbClient
	streamCh       <-chan llm.StreamEvent
	ctx            context.Context
	width          int
	height         int
	waiting        bool
	err            error
}

type streamEventMessage struct {
	event llm.StreamEvent
}

type streamClosedMessage struct {
}

type conversationSavedMessage struct {
	conversation model.Conversation
	err          error
}

type conversationRenamedMessage struct {
	title   string
	summary string
	err     error
}

func InitialModel(ctx context.Context, config config.Config, client history.DbClient) uiModel {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.SetVirtualCursor(false)
	ta.Focus()

	ta.Prompt = "｜"
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

	sp := spinner.New()
	sp.Spinner = spinner.Monkey

	ui := uiModel{
		textarea:     ta,
		conversation: model.Conversation{},
		viewport:     vp,
		spinner:      sp,
		senderStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		errorStyle:   lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
		agentStyle:   lipgloss.NewStyle().Foreground(lipgloss.Color("86")),
		client:       *llm.NewLLMClient(config),
		focus:        focusChat,
		history:      client,
		ctx:          ctx,
		logger:       *slog.Default().With("Component", "TUI"),
		err:          nil,
	}
	ui.picker = newPicker()
	ui.commands = newCommandList(30)
	ui.commands.setAvailable(ui.buildCommands())
	return ui
}

func (m uiModel) Init() tea.Cmd {
	return textarea.Blink
}

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.logger.Debug("Received message", "msg", msg)
	if m.waiting {
		m.spinner, _ = m.spinner.Update(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.SetWidth(msg.Width * 80 / 100)
		m.textarea.SetWidth(msg.Width * 80 / 100)
		m.viewport.SetHeight(msg.Height - lipgloss.Height(m.textarea.View()) - 1)
		m.width = msg.Width
		m.height = msg.Height
		m.picker.SetSize(msg.Width, msg.Height)
		m.commands.list.SetSize(msg.Width*80/100, commandDropdownHeight)
		if m.commands.open {
			m.viewport.SetHeight(m.viewport.Height() - commandDropdownHeight)
		}

		var messages = m.renderMessages()

		if len(m.conversation.Messages) > 0 {
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
		} else {
			m.viewport.SetContent(renderLanding(m.viewport.Width(), m.viewport.Height()))
		}
		m.logger.Debug("New sizes",
			"vp width", m.viewport.Width(), "vp height", m.viewport.Height(),
			"ta width", m.textarea.Width(), "ta height", m.textarea.Height())
		m.viewport.GotoBottom()

	case tea.KeyPressMsg:
		if m.focus != focusChat {
			picker, cmd := m.picker.Update(msg)
			m.picker = picker
			return m, cmd
		}
		switch msg.String() {
		case "esc":
			if m.commands.open {
				m.closeCommands()
				return m, nil
			}
			if m.focus != focusChat {
				m.focus = focusChat
			}
			return m, nil
		case "ctrl+c":
			fmt.Println(m.textarea.Value())
			return m, tea.Quit
		case "shift+enter", "ctrl+j", "alt+enter":
			m.textarea.InsertRune('\n')
			return m, nil
		case "up", "down":
			if m.commands.open {
				var cmd tea.Cmd
				m.commands.list, cmd = m.commands.list.Update(msg)
				return m, cmd
			}
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		case "tab":
			if m.commands.open {
				if sel, ok := m.commands.selectedItem(); ok {
					m.textarea.SetValue("/" + sel.name + " ")
				}
				m.closeCommands()
				return m, nil
			}
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			return m, cmd
		case "enter":
			m.setStatusMessage("")
			input := m.textarea.Value()

			if m.commands.open {
				if sel, ok := m.commands.selectedItem(); ok {
					m.textarea.SetValue("")
					m.closeCommands()
					mm, cmd := sel.handler(m)
					return mm, cmd
				}
			}

			if after, ok := strings.CutPrefix(input, "/"); ok {
				name := after
				for _, c := range m.commands.available {
					if c.name == name {
						m.textarea.SetValue("")
						m.closeCommands()
						mm, cmd := c.handler(m)
						return mm, cmd
					}
				}
			}

			m.textarea.SetValue("")
			m.closeCommands()
			m.logger.Debug("Received enter. Creating new message", "message", input)

			m.conversation.Messages = append(m.conversation.Messages, model.Message{
				Content: input,
				Role:    "user",
				Seq:     len(m.conversation.Messages) + 1,
			})
			var messages = m.renderMessages()
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
			m.textarea.Reset()
			m.viewport.GotoBottom()
			m.startWait()
			return m, tea.Batch(m.callAgent(m.conversation.Messages), m.saveConversation(), m.spinner.Tick)
		default:
			var cmd tea.Cmd
			m.textarea, cmd = m.textarea.Update(msg)
			v := m.textarea.Value()
			if strings.HasPrefix(v, "/") {
				m.openCommands()
			} else {
				m.closeCommands()
			}
			return m, cmd
		}

	case cursor.BlinkMsg:
		m.logger.Debug("Received BlinkMsg")
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd

	case conversationRenamedMessage:
		m.logger.Debug("Received messageRenamedMessage")
		m.startWait()
		if msg.err != nil {
			m.logger.Error("There was an error", "error", msg.err)
			m.setErrorMessage(msg.err.Error())
		}
		m.setStatusMessage("Conversation renamed to: " + msg.title)
		m.conversation.Title = msg.title
		m.conversation.Summary = msg.summary
		return m, nil

	case streamEventMessage:
		m.logger.Debug("Received streamEventMessage")
		if msg.event.Err != nil {
			m.logger.Error("There was an error mid stream", "error", msg.event.Err)
		}

		if msg.event.Reasoning != "" {
			m.thinkingBuffer.WriteString(msg.event.Reasoning)
		}

		if msg.event.Content != "" {
			m.answerBuffer.WriteString(msg.event.Content)
		}

		if msg.event.Done {
			for _, r := range msg.event.Messages {
				r.Seq = len(m.conversation.Messages) + 1
				m.logger.Debug("Appending response message", "message", r)
				m.conversation.Messages = append(m.conversation.Messages, r)
			}

			var messages = m.renderMessages()
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
			m.viewport.GotoBottom()
			return m, m.saveConversation()
		}

		return m, waitForEvent(m.streamCh)

	case streamClosedMessage:
		m.logger.Debug("Received streamClosedMessage")
		m.stopWait()
		m.answerBuffer.Reset()
		m.thinkingBuffer.Reset()

		return m, nil

	case pickerChosenMsg:
		switch m.focus {
		case focusModels:
			m.logger.Debug("Received pickerChosenMsg", "model", msg.id)
			m.client.Cfg.Llm.Model = msg.id
			m.stopWait()
			m.focus = focusChat
		case focusHistory:
			m.logger.Debug("Received pickerChosenMsg", "history", msg.id)
			conv, err := m.history.GetConversation(m.ctx, msg.id)
			if err != nil {
				m.logger.Error("Could not retrieve conversation", "id", msg.id)
			}
			m.conversation = conv
			m.stopWait()
			m.focus = focusChat
			var messages = m.renderMessages()
			m.viewport.SetContent(lipgloss.NewStyle().Width(m.viewport.Width()).Render(messages))
			m.viewport.GotoBottom()
		}

	case pickerDismissMsg:
		m.logger.Debug("Received pickerDismissMsg")
		m.stopWait()
		m.focus = focusChat

	case conversationSavedMessage:
		m.logger.Debug("Received conversationSavedMessage", "conversation", msg.conversation)
		if msg.err != nil {
			m.logger.Error("Could not save message", "error", msg.err)
			m.setErrorMessage(msg.err.Error())
			return m, nil
		}
		m.conversation = msg.conversation
	}

	if m.focus != focusChat {
		picker, cmd := m.picker.Update(msg)
		m.picker = picker
		return m, cmd
	}

	return m, nil
}

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
	parts = append(parts, m.textarea.View())
	ui := lipgloss.JoinVertical(
		lipgloss.Left,
		parts...)

	c := m.textarea.Cursor()
	if c != nil {
		c.Y += lipgloss.Height(m.viewport.View())
		if m.statusMessage != "" {
			c.Y++
		}
		if m.commands.open {
			c.Y += lipgloss.Height(m.commands.view())
		}
		if m.waiting {
			c.Y++
		}
		gap := m.width - lipgloss.Width(ui)
		c.X += max(gap/2, 0)
	}
	view := tea.NewView(lipgloss.PlaceHorizontal(m.width, lipgloss.Center, ui))
	view.Cursor = c
	view.AltScreen = true
	return view
}

func (m uiModel) callAgent(messages []model.Message) tea.Cmd {
	m.logger.Debug("Calling agent", "messages", messages)
	return func() tea.Msg {
		resp, err := m.client.QueryStream(messages)
		if err != nil {
			m.logger.Error("Error while calling stream query", "error", err)
			return streamClosedMessage{}
		}
		return waitForEvent(resp)
	}
}

func waitForEvent(ch <-chan llm.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMessage{}
		}
		return streamEventMessage{ev}
	}
}

func (m uiModel) saveConversation() tea.Cmd {
	m.logger.Debug("Saving conversation", "conversation", m.conversation)
	return func() tea.Msg {
		conv, err := m.history.SaveConversation(m.ctx, m.conversation)
		return conversationSavedMessage{conversation: conv, err: err}
	}
}

func (m uiModel) renameConversation() tea.Cmd {
	m.logger.Debug("Renaming conversation", "conversation", m.conversation)
	return func() tea.Msg {
		meta, err := m.client.Rename(m.conversation.Messages)
		if err != nil {
			return conversationRenamedMessage{title: meta.Title, summary: meta.Summary, err: err}
		}
		m.conversation.Title = meta.Title
		m.conversation.Summary = meta.Summary
		m.conversation, err = m.history.SaveConversation(m.ctx, m.conversation)

		return conversationRenamedMessage{title: m.conversation.Title, summary: m.conversation.Summary, err: err}
	}
}

func (m uiModel) getModels() tea.Cmd {
	m.logger.Debug("Querying models")
	return func() tea.Msg {
		resp, err := m.client.Models()
		if err != nil {
			m.logger.Error("Could not fetch models", "error", err)
		}
		m.logger.Debug("Received models from llm", "models", resp)
		items := []modelItem{}
		for _, model := range resp {
			items = append(items, modelItem{name: model, description: model})
		}
		return modelsLoadedMsg{models: items, title: "Choose a model"}
	}
}

func (m uiModel) getHistory() tea.Cmd {
	m.logger.Debug("Querying history")
	return func() tea.Msg {
		resp, err := m.history.GetConversations(m.ctx)
		if err != nil {
			m.logger.Error("Could not fetch history", "error", err)
		}
		m.logger.Debug("Received conversations from psql", "conversations", resp)
		items := []modelItem{}
		for _, conv := range resp {
			name := conv.Id
			if conv.Title != "" {
				name = conv.Title
			}
			items = append(items, modelItem{name: name, description: conv.Summary})
		}
		return modelsLoadedMsg{models: items, title: "Select a conversation to continue from where you left off"}
	}
}

func (m *uiModel) renderMessages() string {
	var result strings.Builder
	var width = m.viewport.Width()
	for _, message := range m.conversation.Messages {
		switch message.Role {
		case "user":
			var line strings.Builder
			line.WriteString(m.senderStyle.Render("You: "))
			line.WriteString(message.Content)
			result.WriteString(lipgloss.PlaceHorizontal(width, lipgloss.Right, line.String()))
			result.WriteString("\n")

			var sepStyle lipgloss.Style = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				MarginTop(1).
				MarginBottom(1)
			sep := sepStyle.Render(strings.Repeat("┈", width))
			result.WriteString(sep)
			result.WriteString("\n")
		case "assistant":
			result.WriteString(m.agentStyle.Render("Agent: "))
			result.WriteString(message.Content)
			result.WriteString("\n")

			var sepStyle lipgloss.Style = lipgloss.NewStyle().
				Foreground(lipgloss.Color("240")).
				MarginTop(1).
				MarginBottom(1)
			sep := sepStyle.Render(strings.Repeat("┈", width))
			result.WriteString(sep)
			result.WriteString("\n")
		}
	}
	if m.answerBuffer.String() != "" {
		result.WriteString(m.agentStyle.Render("Agent: "))
		result.WriteString(m.answerBuffer.String())

		result.WriteString("\n")
		var sepStyle lipgloss.Style = lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			MarginTop(1).
			MarginBottom(1)
		sep := sepStyle.Render(strings.Repeat("┈", width))
		result.WriteString(sep)
		result.WriteString("\n")
	}
	if m.thinkingBuffer.String() != "" && m.answerBuffer.String() == "" {
		result.WriteString(m.agentStyle.Render("Thinking: "))
		result.WriteString(m.answerBuffer.String())

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

func (m *uiModel) startWait() {
	if !m.waiting {
		m.viewport.SetHeight(m.viewport.Height() - 1)
		m.waiting = true
	}
}

func (m *uiModel) stopWait() {
	if m.waiting {
		m.waiting = false
		m.viewport.SetHeight(m.viewport.Height() - 1)
	}
}

func (m *uiModel) setErrorMessage(message string) {
	m.statusMessage = m.errorStyle.Render(message)
}

func (m *uiModel) setStatusMessage(message string) {
	if message == "" {
		m.statusMessage = m.statusStyle.Render("model: " + m.client.Cfg.Llm.Model + " url: " + m.client.Cfg.Api.Url)
		return
	}
	m.statusMessage = m.statusStyle.Render(message)
}

func (m *uiModel) openCommands() {
	if !m.commands.open {
		m.commands.open = true
		m.viewport.SetHeight(m.viewport.Height() - commandDropdownHeight)
	}
	m.commands.filter(strings.TrimPrefix(m.textarea.Value(), "/"))
}

func (m *uiModel) closeCommands() {
	if m.commands.open {
		m.commands.open = false
		m.viewport.SetHeight(m.viewport.Height() + commandDropdownHeight)
	}
}
