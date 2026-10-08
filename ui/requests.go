package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	llm "git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func (m uiModel) callAgent(ctx context.Context, messages []model.Message) tea.Cmd {
	m.logger.Debug("Calling agent", "messages", len(messages))
	return func() tea.Msg {
		resp, err := m.client.QueryStream(ctx, messages, m.tools.Specs())
		if err != nil {
			m.logger.Error("Error while calling stream query", "error", err)
			return streamClosedMessage{err: err}
		}
		return streamStartedMessage{ch: resp}
	}
}

func (m uiModel) runTools(ctx context.Context, calls []model.ToolCall) tea.Cmd {
	return func() tea.Msg {
		var results []model.Message
		for _, call := range calls {
			if ctx.Err() != nil {
				break
			}
			results = append(results, m.tools.Call(ctx, call))
		}
		return toolsDoneMessage{results: results}
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

func (m *uiModel) requestSave() tea.Cmd {
	if m.saving {
		m.saveQueued = true
		return nil
	}
	m.saving = true
	conv := m.conversation
	m.logger.Debug("Saving conversation", "id", conv.Id, "messages", len(conv.Messages))
	return func() tea.Msg {
		saved, err := m.history.SaveConversation(m.ctx, conv)
		return conversationSavedMessage{conversation: saved, err: err}
	}
}

func (m uiModel) renameConversation(auto bool) tea.Cmd {
	messages := m.conversation.Messages
	gen := m.convGen
	return func() tea.Msg {
		meta, err := m.client.Rename(m.ctx, messages)
		return conversationRenamedMessage{title: meta.Title, summary: meta.Summary, err: err, gen: gen, auto: auto}
	}
}

func (m uiModel) getModels() tea.Cmd {
	m.logger.Debug("Querying models")
	return func() tea.Msg {
		resp, err := m.client.Models(m.ctx)
		if err != nil {
			m.logger.Error("Could not fetch models", "error", err)
			return modelsLoadedMsg{err: err}
		}
		m.logger.Debug("Received models from llm", "models", resp)
		items := []modelItem{}
		for _, model := range resp {
			items = append(items, modelItem{id: model, name: model, description: model})
		}
		return modelsLoadedMsg{models: items, title: "Choose a model"}
	}
}

func (m uiModel) searchConversations(query string) tea.Cmd {
	return func() tea.Msg {
		hits, err := m.searcher.Search(m.ctx, query, search.Options{})
		if err != nil {
			return modelsLoadedMsg{err: err}
		}
		items := []modelItem{}
		for _, hit := range hits {
			name := hit.ConversationTitle
			if name == "" {
				name = hit.ConversationId
			}
			snippet := strings.Join(strings.Fields(hit.Snippet), " ")
			items = append(items, modelItem{id: hit.ConversationId, name: name, description: hit.Role + ": " + snippet})
		}
		return modelsLoadedMsg{models: items, title: "Results for " + query}
	}
}

func (m uiModel) getHistory() tea.Cmd {
	m.logger.Debug("Querying history")
	return func() tea.Msg {
		resp, err := m.history.GetConversations(m.ctx)
		if err != nil {
			m.logger.Error("Could not fetch history", "error", err)
			return modelsLoadedMsg{err: err}
		}
		m.logger.Debug("Received conversations from psql", "count", len(resp))
		items := []modelItem{}
		for _, conv := range resp {
			name := conv.Id
			if conv.Title != "" {
				name = conv.Title
			}
			items = append(items, modelItem{id: conv.Id, name: name, description: conv.Summary})
		}
		return modelsLoadedMsg{models: items, title: "Select a conversation to continue from where you left off"}
	}
}
