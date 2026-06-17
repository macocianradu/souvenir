package main

import (
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/history"
	ui "git.estatecloud.org/radumaco/souvenir/ui"
)


func main() {
	config, err := config.Load(".config.json")
	if err != nil {
		slog.Error("Config error:", "message", err.Error())
		os.Exit(1)
	}
	var logger = slog.Default().With("Compoent", "Main")
	logger.Debug("Config initialized")

	db := history.DbClient{Cfg: config.Db, Logger: *slog.Default().With("Component", "History")};
	err = db.Init()
	if err != nil {
		logger.Error("Error while initializing database", "message", err)
		os.Exit(1)
	}

	p := tea.NewProgram(ui.InitialModel(*config, db))
	if _, err := p.Run(); err != nil {
		logger.Error("Alas, there's been an error:", "message", err)
		os.Exit(1)
	}
}
