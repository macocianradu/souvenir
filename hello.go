package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db"
	"git.estatecloud.org/radumaco/souvenir/db/embed"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	"git.estatecloud.org/radumaco/souvenir/llm"
	ui "git.estatecloud.org/radumaco/souvenir/ui"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config, err := config.Load(".config.json")
	if err != nil {
		fatal("Config error", err)
	}
	var logger = slog.Default().With("Component", "Main")
	logger.Debug("Config initialized")

	pool, err := db.Open(ctx, config.Db)
	if err != nil {
		fatal("Error while initializing database", err)
	}
	defer pool.Close()
	historyClient := history.New(pool, config.Db)
	store := startEmbedding(ctx, config, pool, logger)
	searcher := search.New(pool, store)

	p := tea.NewProgram(ui.InitialModel(ctx, *config, *historyClient, searcher))
	if _, err := p.Run(); err != nil {
		fatal("Alas, there's been an error", err)
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}

func startEmbedding(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *embed.EmbedStore {
	embedder := llm.NewEmbedder(cfg.Embedding)
	store, err := embed.NewEmbedStore(ctx, pool, cfg.Db.DbName, *embedder)
	if err != nil {
		logger.Error("Embedding disabled", "error", err)
		return nil
	}
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.Embedding.Interval) * time.Minute)
		defer ticker.Stop()
		for {
			runEmbedding(ctx, store, cfg.Embedding.Quiet, logger)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return store
}

func runEmbedding(ctx context.Context, em *embed.EmbedStore, quietMinutes int, logger *slog.Logger) {
	convs, err := em.GetConversationsToEmbed(ctx, quietMinutes)
	if err != nil {
		logger.Error("Could not get conversations to embed", "error", err)
		return
	}
	for _, conv := range convs {
		if err := em.EmbedConversation(ctx, conv); err != nil {
			logger.Error("Could not embed conversation", "conversation_id", conv, "error", err)
		}
	}
}
