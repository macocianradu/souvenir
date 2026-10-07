package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db"
	"git.estatecloud.org/radumaco/souvenir/db/embed"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	"git.estatecloud.org/radumaco/souvenir/llm"
	ui "git.estatecloud.org/radumaco/souvenir/ui"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config, err := config.Load(".config.json")
	if err != nil {
		slog.Error("Config error:", "message", err.Error())
		os.Exit(1)
	}
	var logger = slog.Default().With("Component", "Main")
	logger.Debug("Config initialized")

	pool, err := db.Open(ctx, config.Db)
	if err != nil {
		logger.Error("Error while initializing database", "message", err)
		os.Exit(1)
	}
	defer pool.Close()
	historyClient := history.New(pool, config.Db)
	startEmbedding(ctx, config, pool, logger)

	p := tea.NewProgram(ui.InitialModel(ctx, *config, *historyClient))
	if _, err := p.Run(); err != nil {
		logger.Error("Alas, there's been an error:", "message", err)
		os.Exit(1)
	}
}

func startEmbedding(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) {
	embedder := llm.NewEmbedder(cfg.Embedding)
	store, err := embed.NewEmbedStore(ctx, pool, cfg.Db.DbName, *embedder)
	if err != nil {
		logger.Error("Embedding disabled", "error", err)
		return
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
