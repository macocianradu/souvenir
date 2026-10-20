package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/news"
)

const newsEntryChars = 6000

func NewsTools(client *news.Client, allowed []string) Provider {
	all := newsTools(client)
	return Provider{
		Catalog: all,
		Tools: func(ctx context.Context) []Tool {
			if u, ok := users.FromContext(ctx); ok && slices.Contains(allowed, u.Name) {
				return all
			}
			return nil
		},
	}
}

func newsTools(client *news.Client) []Tool {
	return []Tool{
		{
			Name:      "news_feeds",
			Untrusted: true,
			Description: "List the news feeds the user subscribes to in their feed reader, with their categories. " +
				"Use it to find which feed or category to read from.",
			Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				feeds, err := client.Feeds(ctx)
				if err != nil {
					return "", err
				}
				if len(feeds) == 0 {
					return "No feeds.", nil
				}
				byCategory := map[string][]string{}
				var order []string
				for _, f := range feeds {
					c := f.Category.Title
					if _, ok := byCategory[c]; !ok {
						order = append(order, c)
					}
					byCategory[c] = append(byCategory[c], fmt.Sprintf("%s [feed_id: %d]", oneLine(f.Title), f.Id))
				}
				slices.Sort(order)
				var b strings.Builder
				for _, c := range order {
					fmt.Fprintf(&b, "%s:\n", c)
					for _, f := range byCategory[c] {
						b.WriteString("- " + f + "\n")
					}
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:      "news_list",
			Untrusted: true,
			Description: "List entries from the user's subscribed news feeds, newest first, with a short excerpt. " +
				"Use it for \"what's new\", a summary of the news, or what a feed or category has published lately, " +
				"and try it before web_search for recent news on a topic. " +
				"Read one entry in full with news_get.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"search": {"type": "string", "description": "Only entries containing these words"},
					"category": {"type": "string", "description": "Only entries from the category with this name, as shown by news_feeds"},
					"feed_id": {"type": "integer", "description": "Only entries from this feed, from news_feeds"},
					"status": {"type": "string", "enum": ["unread", "read"], "description": "Leave out for both"},
					"starred": {"type": "boolean", "description": "Only starred entries"},
					"days": {"type": "integer", "description": "Only entries published in the last this many days"},
					"limit": {"type": "integer", "description": "Maximum number of entries, 1 to 50. Default 20"}
				}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Search   string `json:"search"`
					Category string `json:"category"`
					FeedId   int64  `json:"feed_id"`
					Status   string `json:"status"`
					Starred  bool   `json:"starred"`
					Days     int    `json:"days"`
					Limit    int    `json:"limit"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				switch args.Status {
				case "", "unread", "read":
				default:
					return "", fmt.Errorf("status must be unread or read, not %q", args.Status)
				}
				q := news.Query{Search: strings.TrimSpace(args.Search), Status: args.Status, Starred: args.Starred, FeedId: args.FeedId, Limit: 20}
				if args.Limit > 0 {
					q.Limit = min(args.Limit, 50)
				}
				if args.Days > 0 {
					q.After = time.Now().AddDate(0, 0, -args.Days)
				}
				if args.Category != "" {
					id, err := categoryId(ctx, client, args.Category)
					if err != nil {
						return "", err
					}
					q.CategoryId = id
				}
				found, err := client.Entries(ctx, q)
				if err != nil {
					return "", err
				}
				if len(found.Entries) == 0 {
					return "No matching entries.", nil
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%d of %d matching entries:\n\n", len(found.Entries), found.Total)
				for _, e := range found.Entries {
					b.WriteString(entryHeader(e))
					if excerpt := truncate(oneLine(entryText(e.Content)), 300); excerpt != "" {
						b.WriteString(excerpt + "\n")
					}
					b.WriteString("\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:        "news_get",
			Untrusted:   true,
			Description: "Read one entry from the user's news feeds in full, by its entry_id from news_list.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"entry_id": {"type": "integer"}},
				"required": ["entry_id"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					EntryId int64 `json:"entry_id"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if args.EntryId <= 0 {
					return "", errors.New("entry_id is required")
				}
				e, err := client.Entry(ctx, args.EntryId)
				if err != nil {
					return "", err
				}
				text := entryText(e.Content)
				if text == "" {
					text = "The entry has no text in the feed; read its link with web_fetch."
				} else if len([]rune(text)) > newsEntryChars {
					text = truncate(text, newsEntryChars) + "\n\n[Cut short; read the link with web_fetch for the rest.]"
				}
				return entryHeader(e) + "\n" + text, nil
			},
		},
	}
}

func categoryId(ctx context.Context, client *news.Client, name string) (int64, error) {
	categories, err := client.Categories(ctx)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, c := range categories {
		if strings.EqualFold(strings.TrimSpace(c.Title), strings.TrimSpace(name)) {
			return c.Id, nil
		}
		names = append(names, c.Title)
	}
	return 0, fmt.Errorf("no category %q; categories: %s", name, strings.Join(names, ", "))
}

func entryHeader(e news.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [entry_id: %d]\n", oneLine(e.Title), e.Id)
	fmt.Fprintf(&b, "%s, %s", oneLine(e.Feed.Title), e.PublishedAt.Format("Mon 2006-01-02 15:04"))
	var notes []string
	if e.Status == "unread" {
		notes = append(notes, "unread")
	}
	if e.Starred {
		notes = append(notes, "starred")
	}
	if len(notes) > 0 {
		b.WriteString(" (" + strings.Join(notes, ", ") + ")")
	}
	return b.String() + "\n" + e.Url + "\n"
}

func entryText(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	md, err := htmltomarkdown.ConvertString(content)
	if err != nil {
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(md)
}
