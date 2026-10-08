package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/db/memory"
)

func MemoryTools(store *memory.Store) []Tool {
	return []Tool{
		{
			Name: "memory_save",
			Description: "Save a durable fact for future conversations: the user's preferences, personal details, " +
				"people and places in their life, ongoing projects and decisions. One self-contained fact per call, " +
				"written in the third person (\"The user is vegetarian\"). Do not save small talk or things that only " +
				"matter in this conversation. Search first to avoid saving what is already known.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"content": {"type": "string", "description": "The fact, as one self-contained sentence"}},
				"required": ["content"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if strings.TrimSpace(args.Content) == "" {
					return "", errors.New("content is required")
				}
				m, created, err := store.Save(ctx, args.Content, conversationFrom(ctx))
				if err != nil {
					return "", err
				}
				if !created {
					return "Already remembered as " + m.Id + ".", nil
				}
				return "Saved as " + m.Id + ".", nil
			},
		},
		{
			Name: "memory_search",
			Description: "Search the facts saved about the user with memory_save, by meaning and by keyword. " +
				"Use it before answering anything that depends on who the user is or what they told you before.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "What to look for"},
					"limit": {"type": "integer", "description": "Maximum number of memories, 1 to 50. Default 10"}
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
					limit = 10
				}
				found, err := store.Search(ctx, args.Query, min(limit, 50))
				if err != nil {
					return "", err
				}
				if len(found) == 0 {
					return "No matching memories.", nil
				}
				var b strings.Builder
				for _, m := range found {
					fmt.Fprintf(&b, "- [%s] %s (saved %s)\n", m.Id, m.Content, m.CreatedAt.Format("2006-01-02"))
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name: "memory_forget",
			Description: "Delete a saved memory by its id, when the user asks you to forget it or it turned out " +
				"wrong or outdated. Find the id with memory_search. To correct a fact, forget it and save the new one.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"id": {"type": "string", "description": "The memory id from memory_search"}},
				"required": ["id"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Id string `json:"id"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if err := store.Forget(ctx, args.Id); err != nil {
					return "", err
				}
				return "Forgotten.", nil
			},
		},
	}
}
