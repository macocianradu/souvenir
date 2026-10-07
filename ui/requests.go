package ui

import (
	tea "charm.land/bubbletea/v2"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func (m uiModel) callAgent(messages []model.Message) tea.Cmd {
	m.logger.Debug("Calling agent", "messages", messages)
	return func() tea.Msg {
		resp, err := m.client.QueryStream(messages)
		if err != nil {
			m.logger.Error("Error while calling stream query", "error", err)
			return streamClosedMessage{}
		}
		return streamStartedMessage{ch: resp}
	}
}

func waitForEvent(ch <-chan llm.StreamEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMessage{}
		}
		return streamEventMessage{event: ev}
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
