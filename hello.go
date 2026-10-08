package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

type app struct {
	cfg      *config.Config
	history  *history.DbClient
	searcher *search.Searcher
	registry *tools.Registry
	memories *memory.Store
}

func (a app) newModel(ctx context.Context) tea.Model {
	return ui.InitialModel(ctx, *a.cfg, *a.history, a.searcher, a.registry, a.memories)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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
	a := app{
		cfg:      config,
		history:  history.New(pool, config.Db),
		searcher: searcher,
		registry: tools.NewRegistry(append([]tools.Tool{tools.SearchHistory(searcher)}, tools.MemoryTools(memories)...)...),
		memories: memories,
	}

	switch mode := strings.Join(os.Args[1:], " "); mode {
	case "":
		p := tea.NewProgram(a.newModel(ctx))
		if _, err := p.Run(); err != nil {
			fatal("Alas, there's been an error", err)
		}
	case "serve":
		if err := serve(ctx, config.Ssh, a.newModel); err != nil {
			fatal("SSH server error", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: souvenir [serve]\n", mode)
		os.Exit(2)
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
