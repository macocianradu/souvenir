package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/agent"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/memory"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func waitForEvent(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamClosedMessage{}
		}
		return agentEventMessage{event: ev}
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

func (m uiModel) summarizeConversation() tea.Cmd {
	conv := m.conversation
	gen := m.convGen
	pending := conv.Unsummarized()
	fold := pending[:len(pending)-m.client.Cfg.Llm.KeepRecent]
	previous := ""
	if conv.ContextSummary != nil {
		previous = conv.ContextSummary.Content
	}
	return func() tea.Msg {
		content, err := m.client.Summarize(m.ctx, previous, fold)
		if err != nil {
			return summarizedMessage{err: err, gen: gen}
		}
		summary := &model.ContextSummary{ThroughSeq: fold[len(fold)-1].Seq, Content: content}
		if err := m.history.SaveContextSummary(m.ctx, conv.Id, *summary); err != nil {
			return summarizedMessage{err: err, gen: gen}
		}
		return summarizedMessage{summary: summary, gen: gen}
	}
}

func (m uiModel) deleteConversation(item modelItem) tea.Cmd {
	return func() tea.Msg {
		return itemDeletedMessage{item: item, err: m.history.DeleteConversation(m.ctx, item.id)}
	}
}

func (m uiModel) getMemories() tea.Cmd {
	return func() tea.Msg {
		all, err := m.memories.List(m.ctx)
		if err != nil {
			return modelsLoadedMsg{err: err}
		}
		return modelsLoadedMsg{models: memoryItems(all), title: "What the assistant remembers", searchable: true,
			warning: "Forget %q permanently? It cannot be recovered."}
	}
}

func (m uiModel) searchMemories(query string) tea.Cmd {
	return func() tea.Msg {
		found, err := m.memories.Search(m.ctx, query, 50)
		return pickerResultsMsg{query: query, items: memoryItems(found), err: err}
	}
}

func (m uiModel) forgetMemory(item modelItem) tea.Cmd {
	return func() tea.Msg {
		return itemDeletedMessage{item: item, err: m.memories.Forget(m.ctx, item.id)}
	}
}

func memoryItems(memories []memory.Memory) []modelItem {
	items := make([]modelItem, len(memories))
	for i, mem := range memories {
		items[i] = modelItem{id: mem.Id, name: mem.Content, description: "saved " + mem.CreatedAt.Format("2006-01-02")}
	}
	return items
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

func (m uiModel) getEfforts() tea.Cmd {
	return func() tea.Msg {
		items := []modelItem{{id: "", name: "default", description: "Send no reasoning effort and leave it to the model"}}
		for _, effort := range config.ReasoningEfforts {
			items = append(items, modelItem{id: effort, name: effort, description: "reasoning_effort: " + effort})
		}
		return modelsLoadedMsg{models: items, title: "Choose a reasoning effort (now: " + effortName(m.client.Cfg.Llm.ReasoningEffort) + ")"}
	}
}

func effortName(effort string) string {
	if effort == "" {
		return "default"
	}
	return effort
}

func (m uiModel) searchHistory(query string) tea.Cmd {
	return func() tea.Msg {
		hits, err := m.searcher.Search(m.ctx, query, search.Options{Limit: 50})
		if err != nil {
			return pickerResultsMsg{query: query, err: err}
		}
		items := []modelItem{}
		seen := map[string]bool{}
		for _, hit := range hits {
			if seen[hit.ConversationId] {
				continue
			}
			seen[hit.ConversationId] = true
			name := hit.ConversationTitle
			if name == "" {
				name = "Untitled"
			}
			snippet := strings.Join(strings.Fields(hit.Snippet), " ")
			items = append(items, modelItem{id: hit.ConversationId, name: name, description: hit.Role + ": " + snippet})
		}
		return pickerResultsMsg{query: query, items: items}
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
			name := conv.Title
			if name == "" {
				name = truncate(conv.Preview, 60)
			}
			if name == "" {
				name = "Untitled"
			}
			items = append(items, modelItem{id: conv.Id, name: name, description: conv.Summary})
		}
		return modelsLoadedMsg{models: items, title: "Select a conversation to continue from where you left off", searchable: true,
			warning: "Delete %q permanently? Its messages, summaries and search index are removed and cannot be recovered."}
	}
}
