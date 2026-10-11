package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"git.estatecloud.org/radumaco/souvenir/web"
)

func WebSearch(searcher web.Searcher) Tool {
	return Tool{
		Name: "web_search",
		Description: "Search the web. Use it for current events, facts you are unsure of, or anything " +
			"that may have changed since your training. Returns titles, links and short snippets; " +
			"cite the links you rely on.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "The search query"},
				"time_range": {"type": "string", "enum": ["day", "week", "month", "year"], "description": "Only results from this recent period. Leave out for any time"},
				"limit": {"type": "integer", "description": "Maximum number of results, 1 to 10. Default 5"}
			},
			"required": ["query"]
		}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Query     string `json:"query"`
				TimeRange string `json:"time_range"`
				Limit     int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Query) == "" {
				return "", errors.New("query is required")
			}
			switch args.TimeRange {
			case "", "day", "week", "month", "year":
			default:
				return "", fmt.Errorf("time_range must be day, week, month or year, not %q", args.TimeRange)
			}
			limit := args.Limit
			if limit <= 0 {
				limit = 5
			}
			found, err := searcher.Search(ctx, args.Query, args.TimeRange)
			if err != nil {
				return "", err
			}
			return formatResults(found, min(limit, 10)), nil
		},
	}
}

func formatResults(found web.Results, limit int) string {
	if len(found.Answers) == 0 && len(found.Results) == 0 {
		return "No results found."
	}
	var b strings.Builder
	for _, answer := range found.Answers {
		fmt.Fprintf(&b, "Answer: %s\n\n", truncate(answer, 600))
	}
	for i, r := range found.Results[:min(limit, len(found.Results))] {
		fmt.Fprintf(&b, "%d. %s\n%s\n", i+1, r.Title, r.Url)
		if r.Published != "" {
			fmt.Fprintf(&b, "Published %s\n", r.Published)
		}
		if r.Content != "" {
			b.WriteString(truncate(r.Content, 400) + "\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
