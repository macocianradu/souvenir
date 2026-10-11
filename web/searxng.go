package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
)

type Searxng struct {
	logger slog.Logger
	url    string
	client *http.Client
}

func NewSearxng(cfg config.WebSearchConfig) *Searxng {
	return &Searxng{
		logger: *slog.Default().With("Component", "WebSearch"),
		url:    strings.TrimSuffix(cfg.Url, "/"),
		client: &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Millisecond},
	}
}

func (s *Searxng) Search(ctx context.Context, query, timeRange string) (Results, error) {
	params := url.Values{"q": {query}, "format": {"json"}}
	if timeRange != "" {
		params.Set("time_range", timeRange)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", s.url+"/search?"+params.Encode(), nil)
	if err != nil {
		return Results{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.logger.Error("Search failed", "error", err)
		return Results{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Results{}, err
	}
	if resp.StatusCode != http.StatusOK {
		body := strings.TrimSpace(string(data[:min(len(data), 512)]))
		s.logger.Error("Search returned an error", "status", resp.Status, "body", body)
		return Results{}, fmt.Errorf("search returned %s", resp.Status)
	}

	var raw struct {
		Results []struct {
			Title         string `json:"title"`
			Url           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"publishedDate"`
		} `json:"results"`
		Answers []json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Results{}, fmt.Errorf("could not read search results: %w", err)
	}
	var out Results
	for _, a := range raw.Answers {
		if answer := answerText(a); answer != "" {
			out.Answers = append(out.Answers, answer)
		}
	}
	for _, r := range raw.Results {
		out.Results = append(out.Results, Result{Title: r.Title, Url: r.Url, Content: r.Content, Published: r.PublishedDate})
	}
	return out, nil
}

func answerText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var obj struct {
		Answer string `json:"answer"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Answer)
	}
	return ""
}
