package web

import (
	"context"
	"fmt"

	"git.estatecloud.org/radumaco/souvenir/config"
)

type Result struct {
	Title     string
	Url       string
	Content   string
	Published string
}

type Results struct {
	Answers []string
	Results []Result
}

type Searcher interface {
	Search(ctx context.Context, query, timeRange string) (Results, error)
}

func New(cfg config.WebSearchConfig) (Searcher, error) {
	switch cfg.Provider {
	case "searxng":
		return NewSearxng(cfg), nil
	default:
		return nil, fmt.Errorf("unknown web search provider %q", cfg.Provider)
	}
}
