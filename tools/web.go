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

func WebFetch(fetcher *web.Fetcher, maxChars int) Tool {
	return Tool{
		Name: "web_fetch",
		Description: "Read a web page or text file by its URL, as markdown with the page's clutter removed. " +
			"Use it on links from web_search when the snippet is not enough, or on a link the user gives. " +
			"Long pages come in parts: the end of each part says which start to ask for next.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"url": {"type": "string", "description": "The http or https URL to read"},
				"start": {"type": "integer", "description": "Character to start from, to read further into a long page. Default 0"}
			},
			"required": ["url"]
		}`),
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Url   string `json:"url"`
				Start int    `json:"start"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			if strings.TrimSpace(args.Url) == "" {
				return "", errors.New("url is required")
			}
			page, err := fetcher.Fetch(ctx, args.Url)
			if err != nil {
				return "", err
			}
			return formatPage(page, max(args.Start, 0), maxChars), nil
		},
	}
}

func formatPage(page web.Page, start, maxChars int) string {
	content := []rune(page.Content)
	var b strings.Builder
	if page.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", page.Title)
	}
	fmt.Fprintf(&b, "URL: %s\n\n", page.Url)
	if len(content) == 0 {
		b.WriteString("The page has no readable text.")
		return b.String()
	}
	if start >= len(content) {
		fmt.Fprintf(&b, "The page is only %d characters long.", len(content))
		return b.String()
	}
	end := min(start+maxChars, len(content))
	b.WriteString(string(content[start:end]))
	if start > 0 || end < len(content) {
		fmt.Fprintf(&b, "\n\n[Characters %d to %d of %d.", start, end, len(content))
		if end < len(content) {
			fmt.Fprintf(&b, " Call web_fetch with start=%d for more.", end)
		}
		b.WriteString("]")
	}
	return b.String()
}
