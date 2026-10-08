package ui

import (
	"context"
	"errors"
	"slices"
	"strings"

	"charm.land/bubbles/v2/cursor"
	tea "charm.land/bubbletea/v2"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func (m uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case cursor.BlinkMsg:
		return m.updateTextarea(msg)

	case tea.MouseWheelMsg:
		if m.focus == focusChat {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

	case conversationRenamedMessage:
		return m.handleRenamed(msg)

	case streamStartedMessage:
		m.streamCh = msg.ch
		return m, waitForEvent(m.streamCh)

	case streamEventMessage:
		return m.handleStreamEvent(msg.event)

	case streamClosedMessage:
		return m.handleStreamClosed(msg)

	case modelsLoadedMsg:
		if msg.err != nil {
			m.focus = focusChat
			m.setErrorMessage("Could not load list: " + msg.err.Error())
			return m, nil
		}
		if m.focus == focusSearch && len(msg.models) == 0 {
			m.focus = focusChat
			m.setStatusMessage("No matches")
			return m, nil
		}

	case pickerChosenMsg:
		m.handlePickerChosen(msg)

	case pickerDismissMsg:
		m.focus = focusChat

	case conversationSavedMessage:
		return m.handleSaved(msg)

	case toolsDoneMessage:
		return m.handleToolsDone(msg)

	case summarizedMessage:
		m.summarizing = false
		if msg.err != nil {
			m.logger.Error("Could not summarize conversation", "error", msg.err)
		} else if msg.gen == m.convGen {
			m.conversation.ContextSummary = msg.summary
		}
		return m, nil
	}

	var spinnerCmd tea.Cmd
	if m.pending > 0 {
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
		if m.commands.open {
			m.closeCommands()
		} else if m.streaming && m.cancelStream != nil {
			m.cancelStream()
		}
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "pgup":
		m.viewport.PageUp()
		return m, nil
	case "pgdown":
		m.viewport.PageDown()
		return m, nil
	case "shift+up":
		m.viewport.ScrollUp(1)
		return m, nil
	case "shift+down":
		m.viewport.ScrollDown(1)
		return m, nil
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

	if after, ok := strings.CutPrefix(strings.TrimSpace(input), "/"); ok {
		name, args, _ := strings.Cut(after, " ")
		args = strings.TrimSpace(args)
		if m.commands.open && args == "" {
			if sel, ok := m.commands.selectedItem(); ok {
				return m.runCommand(sel, "")
			}
		}
		for _, c := range m.commands.available {
			if c.name == name {
				return m.runCommand(c, args)
			}
		}
		m.setErrorMessage("Unknown command /" + name)
		return m, nil
	}

	if strings.TrimSpace(input) == "" {
		return m, nil
	}
	if m.streaming {
		m.setErrorMessage("Wait for the reply to finish before sending another message")
		return m, nil
	}

	m.textarea.Reset()
	m.closeCommands()
	m.logger.Debug("Creating new message", "message", input)

	m.thinkingBuffer.Reset()
	m.appendMessage(model.Message{Content: input, Role: "user"})
	m.refreshViewport()
	m.viewport.GotoBottom()
	m.streaming = true
	m.streamCtx, m.cancelStream = context.WithCancel(m.ctx)
	m.toolRounds = 0
	m.toolTrail = nil
	m.startWait()
	return m, tea.Batch(m.callAgent(m.streamCtx, m.conversation.ContextMessages()), m.requestSave(), m.spinner.Tick)
}

func (m *uiModel) appendMessage(msg model.Message) {
	msg.Seq = len(m.conversation.Messages) + 1
	m.conversation.Messages = append(m.conversation.Messages, msg)
}

func (m uiModel) runCommand(c command, args string) (tea.Model, tea.Cmd) {
	m.textarea.SetValue("")
	m.closeCommands()
	return c.handler(m, args)
}

func (m uiModel) handleStreamEvent(ev llm.StreamEvent) (tea.Model, tea.Cmd) {
	if ev.Err != nil {
		m.logger.Error("There was an error mid stream", "error", ev.Err)
		return m.handleStreamClosed(streamClosedMessage{err: ev.Err})
	}
	m.thinkingBuffer.WriteString(ev.Reasoning)
	m.answerBuffer.WriteString(ev.Content)

	if ev.Done {
		m.answerBuffer.Reset()
		for _, r := range ev.Messages {
			if len(r.ToolCalls) > 0 {
				return m.handleToolCalls(r)
			}
			m.appendMessage(r)
		}
		return m.handleStreamClosed(streamClosedMessage{})
	}

	m.refreshViewport()
	return m, waitForEvent(m.streamCh)
}

func (m uiModel) handleToolCalls(reply model.Message) (tea.Model, tea.Cmd) {
	m.streamCh = nil
	if m.toolRounds >= m.client.Cfg.Llm.MaxToolRounds {
		return m.handleStreamClosed(streamClosedMessage{err: errors.New("tool call limit reached")})
	}
	m.toolRounds++
	m.toolTrail = append(m.toolTrail, reply)
	m.refreshViewport()
	return m, m.runTools(m.streamCtx, reply.ToolCalls)
}

func (m uiModel) handleToolsDone(msg toolsDoneMessage) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	m.toolTrail = append(m.toolTrail, msg.results...)
	if m.streamCtx.Err() != nil {
		return m.handleStreamClosed(streamClosedMessage{})
	}
	m.refreshViewport()
	turn := append(slices.Clone(m.conversation.ContextMessages()), m.toolTrail...)
	return m, m.callAgent(m.streamCtx, turn)
}

func (m uiModel) handleStreamClosed(msg streamClosedMessage) (tea.Model, tea.Cmd) {
	if !m.streaming {
		return m, nil
	}
	cancelled := m.streamCtx.Err() != nil
	m.cancelStream()
	m.streaming = false
	m.streamCh = nil
	m.streamCtx, m.cancelStream = nil, nil
	m.stopWait()
	switch {
	case cancelled:
		m.setStatusMessage("Reply cancelled")
	case msg.err != nil:
		m.setErrorMessage("Request failed: " + msg.err.Error())
	}
	if partial := m.answerBuffer.String(); partial != "" {
		m.appendMessage(model.Message{Role: "assistant", Content: partial})
	}
	m.answerBuffer.Reset()
	m.refreshViewport()
	cmd := m.requestSave()
	if !cancelled && msg.err == nil && m.needsTitle() {
		m.titling = true
		cmd = tea.Batch(cmd, m.renameConversation(true))
	}
	if !cancelled && msg.err == nil && m.needsSummary() {
		m.summarizing = true
		cmd = tea.Batch(cmd, m.summarizeConversation())
	}
	return m, cmd
}

func (m uiModel) needsSummary() bool {
	budget := m.client.Cfg.Llm.ContextBudget
	if m.summarizing || budget == 0 || m.conversation.Id == "" {
		return false
	}
	return len(m.conversation.Unsummarized()) > m.client.Cfg.Llm.KeepRecent &&
		model.EstimateTokens(m.conversation.ContextMessages()) > budget
}

func (m uiModel) needsTitle() bool {
	return !m.titling && m.conversation.Title == "" && m.conversation.TitleSource != model.TitleSourceUser
}

func (m uiModel) handleSaved(msg conversationSavedMessage) (tea.Model, tea.Cmd) {
	m.saving = false
	if msg.err != nil {
		m.logger.Error("Could not save conversation", "error", msg.err)
		m.setErrorMessage("Could not save conversation: " + msg.err.Error())
	} else if m.conversation.Id == "" || m.conversation.Id == msg.conversation.Id {
		m.conversation.Id = msg.conversation.Id
		for i, saved := range msg.conversation.Messages {
			if i < len(m.conversation.Messages) && m.conversation.Messages[i].Seq == saved.Seq {
				m.conversation.Messages[i].Id = saved.Id
			}
		}
	}
	if m.saveQueued {
		m.saveQueued = false
		return m, m.requestSave()
	}
	return m, nil
}

func (m uiModel) handleRenamed(msg conversationRenamedMessage) (tea.Model, tea.Cmd) {
	if msg.auto {
		m.titling = false
	} else {
		m.stopWait()
	}
	if msg.gen != m.convGen {
		return m, nil
	}
	if msg.err != nil {
		m.logger.Error("Could not rename conversation", "auto", msg.auto, "error", msg.err)
		if !msg.auto {
			m.setErrorMessage("Could not rename conversation: " + msg.err.Error())
		}
		return m, nil
	}
	if msg.auto {
		if m.conversation.TitleSource == model.TitleSourceUser {
			return m, nil
		}
	} else {
		m.setStatusMessage("Conversation renamed to: " + msg.title)
	}
	m.conversation.Title = msg.title
	m.conversation.TitleSource = model.TitleSourceLLM
	m.conversation.Summary = msg.summary
	return m, m.requestSave()
}

func (m *uiModel) handlePickerChosen(msg pickerChosenMsg) {
	switch m.focus {
	case focusModels:
		m.logger.Debug("Model chosen", "model", msg.id)
		m.client.Cfg.Llm.Model = msg.id
	case focusHistory, focusSearch:
		m.logger.Debug("Conversation chosen", "history", msg.id)
		conv, err := m.history.GetConversation(m.ctx, msg.id)
		if err != nil {
			m.logger.Error("Could not retrieve conversation", "id", msg.id, "error", err)
			m.setErrorMessage("Could not open conversation: " + err.Error())
			break
		}
		m.openConversation(conv)
	}
	m.focus = focusChat
}

func (m *uiModel) openConversation(conv model.Conversation) {
	m.conversation = conv
	m.convGen++
	m.titling = false
	m.summarizing = false
	m.thinkingBuffer.Reset()
	m.toolTrail = nil
	m.refreshViewport()
	m.viewport.GotoBottom()
}
