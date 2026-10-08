package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db/search"
)

type conversationKey struct{}

func WithConversation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationKey{}, id)
}

func conversationFrom(ctx context.Context) string {
	id, _ := ctx.Value(conversationKey{}).(string)
	return id
}

func SearchHistory(searcher *search.Searcher) Tool {
	return Tool{
		Name: "search_history",
		Description: "Search the user's earlier conversations with you, by meaning and by keyword. " +
			"Use it when the user refers to something discussed before, or when what they said " +
			"in the past would help answer. The current conversation is not included.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "What to look for, in natural language or as keywords"},
				"limit": {"type": "integer", "description": "Maximum number of messages to return, 1 to 20. Default 5"}
			},
			"required": ["query"]
		}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Query) == "" {
				return "", errors.New("query is required")
			}
			limit := args.Limit
			if limit <= 0 {
				limit = 5
			}
			hits, err := searcher.Search(ctx, args.Query, search.Options{
				Limit:                 min(limit, 20),
				ExcludeConversationId: conversationFrom(ctx),
			})
			if err != nil {
				return "", err
			}
			if len(hits) == 0 {
				return "No matching messages found.", nil
			}
			var b strings.Builder
			for i, h := range hits {
				title := h.ConversationTitle
				if title == "" {
					title = "Untitled"
				}
				fmt.Fprintf(&b, "%d. Conversation %q, %s, %s said:\n%s\n\n",
					i+1, title, h.CreatedAt.Format("2006-01-02"), h.Role, truncate(h.Content, 600))
			}
			return strings.TrimSpace(b.String()), nil
		},
	}
}

func truncate(s string, n int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n-1]) + "…"
}
