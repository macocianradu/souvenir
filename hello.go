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
	"git.estatecloud.org/radumaco/souvenir/db/memory"
	"git.estatecloud.org/radumaco/souvenir/db/search"
	"git.estatecloud.org/radumaco/souvenir/llm"
	"git.estatecloud.org/radumaco/souvenir/tools"
	ui "git.estatecloud.org/radumaco/souvenir/ui"
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

	embedder := llm.NewEmbedder(config.Embedding)
	store, err := embed.NewEmbedStore(ctx, pool, config.Db.DbName, *embedder)
	if err != nil {
		logger.Error("Embedding disabled", "error", err)
		embedder = nil
	}
	memories, err := memory.NewStore(ctx, pool, embedder)
	if err != nil {
		logger.Error("Memory embedding disabled", "error", err)
		memories, _ = memory.NewStore(ctx, pool, nil)
	}
	if store != nil {
		go runEmbedding(ctx, config, store, memories, logger)
	}
	searcher := search.New(pool, store)

	registry := tools.NewRegistry(append([]tools.Tool{tools.SearchHistory(searcher)}, tools.MemoryTools(memories)...)...)

	p := tea.NewProgram(ui.InitialModel(ctx, *config, *historyClient, searcher, registry))
	if _, err := p.Run(); err != nil {
		fatal("Alas, there's been an error", err)
	}
}

func runEmbedding(ctx context.Context, cfg *config.Config, store *embed.EmbedStore, memories *memory.Store, logger *slog.Logger) {
	ticker := time.NewTicker(time.Duration(cfg.Embedding.Interval) * time.Minute)
	defer ticker.Stop()
	for {
		convs, err := store.GetConversationsToEmbed(ctx, cfg.Embedding.Quiet)
		if err != nil {
			logger.Error("Could not get conversations to embed", "error", err)
		}
		for _, conv := range convs {
			if err := store.EmbedConversation(ctx, conv); err != nil {
				logger.Error("Could not embed conversation", "conversation_id", conv, "error", err)
			}
		}
		if err := memories.Backfill(ctx); err != nil {
			logger.Error("Could not embed memories", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func fatal(msg string, err error) {
	slog.Error(msg, "error", err)
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}
