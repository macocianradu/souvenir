package ui

import (
	"strings"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.logger.Debug("Received message", "msg", msg)

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case cursor.BlinkMsg:
		return m.updateTextarea(msg)

	case conversationRenamedMessage:
		return m.handleRenamed(msg)

	case streamStartedMessage:
		m.streamCh = msg.ch
		return m, waitForEvent(m.streamCh)

	case streamEventMessage:
		return m.handleStreamEvent(msg.event)

	case streamClosedMessage:
		m.stopWait()
		m.answerBuffer.Reset()
		m.thinkingBuffer.Reset()
		m.refreshViewport()
		return m, nil

	case pickerChosenMsg:
		m.handlePickerChosen(msg)

	case pickerDismissMsg:
		m.stopWait()
		m.focus = focusChat

	case conversationSavedMessage:
		if msg.err != nil {
			m.logger.Error("Could not save message", "error", msg.err)
			m.setErrorMessage(msg.err.Error())
			return m, nil
		}
		m.conversation = msg.conversation
	}

	var spinnerCmd tea.Cmd
	if m.waiting {
		m.spinner, spinnerCmd = m.spinner.Update(msg)
	}

	if m.focus != focusChat {
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		return m, cmd
	}

	return m, spinnerCmd
}

func (m uiModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.focus != focusChat {
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "esc":
		m.closeCommands()
		return m, nil
	case "ctrl+c":
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
		return m.updateTextarea(msg)
	case "tab":
		if m.commands.open {
			if sel, ok := m.commands.selectedItem(); ok {
				m.textarea.SetValue("/" + sel.name + " ")
			}
			m.closeCommands()
			return m, nil
		}
		return m.updateTextarea(msg)
	case "enter":
		return m.submit()
	}

	var cmd tea.Cmd
	m, cmd = m.updateTextarea(msg)
	if strings.HasPrefix(m.textarea.Value(), "/") {
		m.openCommands()
	} else {
		m.closeCommands()
	}
	return m, cmd
}

func (m uiModel) updateTextarea(msg tea.Msg) (uiModel, tea.Cmd) {
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m uiModel) submit() (tea.Model, tea.Cmd) {
	m.setStatusMessage("")
	input := m.textarea.Value()

	if m.commands.open {
		if sel, ok := m.commands.selectedItem(); ok {
			return m.runCommand(sel)
		}
	}
	if name, ok := strings.CutPrefix(input, "/"); ok {
		for _, c := range m.commands.available {
			if c.name == name {
				return m.runCommand(c)
			}
		}
	}

	m.textarea.Reset()
	m.closeCommands()
	m.logger.Debug("Creating new message", "message", input)

	m.conversation.Messages = append(m.conversation.Messages, model.Message{
		Content: input,
		Role:    "user",
		Seq:     len(m.conversation.Messages) + 1,
	})
	m.refreshViewport()
	m.startWait()
	return m, tea.Batch(m.callAgent(m.conversation.Messages), m.saveConversation(), m.spinner.Tick)
}

func (m uiModel) runCommand(c command) (tea.Model, tea.Cmd) {
	m.textarea.SetValue("")
	m.closeCommands()
	return c.handler(m)
}

func (m uiModel) handleStreamEvent(ev llm.StreamEvent) (tea.Model, tea.Cmd) {
	if ev.Err != nil {
		m.logger.Error("There was an error mid stream", "error", ev.Err)
	}
	m.thinkingBuffer.WriteString(ev.Reasoning)
	m.answerBuffer.WriteString(ev.Content)

	if ev.Done {
		for _, r := range ev.Messages {
			r.Seq = len(m.conversation.Messages) + 1
			m.conversation.Messages = append(m.conversation.Messages, r)
		}
		return m, func() tea.Msg { return streamClosedMessage{} }
	}

	m.refreshViewport()
	return m, waitForEvent(m.streamCh)
}

func (m uiModel) handleRenamed(msg conversationRenamedMessage) (tea.Model, tea.Cmd) {
	m.stopWait()
	if msg.err != nil {
		m.logger.Error("There was an error", "error", msg.err)
		m.setErrorMessage(msg.err.Error())
	}
	m.setStatusMessage("Conversation renamed to: " + msg.title)
	m.conversation.Title = msg.title
	m.conversation.Summary = msg.summary
	return m, nil
}

func (m *uiModel) handlePickerChosen(msg pickerChosenMsg) {
	switch m.focus {
	case focusModels:
		m.logger.Debug("Model chosen", "model", msg.id)
		m.client.Cfg.Llm.Model = msg.id
	case focusHistory:
		m.logger.Debug("Conversation chosen", "history", msg.id)
		conv, err := m.history.GetConversation(m.ctx, msg.id)
		if err != nil {
			m.logger.Error("Could not retrieve conversation", "id", msg.id)
		}
		m.conversation = conv
		m.refreshViewport()
	}
	m.focus = focusChat
}
