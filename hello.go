package main

import (
	"context"
	"errors"
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
	ctx := context.Background()
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
	setupEmbedding(config, pool, logger)

	p := tea.NewProgram(ui.InitialModel(ctx, *config, *historyClient))
	if _, err := p.Run(); err != nil {
		logger.Error("Alas, there's been an error:", "message", err)
		os.Exit(1)
	}
}

func setupEmbedding(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) (*embed.EmbedStore, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	embedder := llm.NewEmbedder(cfg.Embedding)
	embedStore, err := embed.NewEmbedStore(ctx, pool, cfg.Db.DbName, *embedder)
	if err != nil {
		logger.Error("Could not initialize embedder", "error", err)
		return nil, err
	}
	ticker := time.NewTicker(time.Duration(cfg.Embedding.Interval) * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runEmbedding(ctx, embedStore, logger)
			}
		}
	}()
	return embedStore, nil
}

func runEmbedding(ctx context.Context, em *embed.EmbedStore, logger *slog.Logger) error {
	convs, err := em.GetConversationsToEmbed(ctx)
	if err != nil {
		logger.Error("Could not get conversations to embed", "error", err)
		return err
	}
	var errs []error
	for _, conv := range convs {
		if err := em.EmbedConversation(ctx, conv); err != nil {
			logger.Error("Could not embed conversation", "conversation_id", conv, "error", err)
			errs = append(errs, err)
		}
	}
	if errs != nil {
		return errors.Join(errs...)
	}
	return nil
}
