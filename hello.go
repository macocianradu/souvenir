package main

import (
	"log/slog"
	"os"

	tea "charm.land/bubbletea/v2"
	"git.estatecloud.org/radumaco/souvenir/config"
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
	p := tea.NewProgram(ui.InitialModel(*config))
	if _, err := p.Run(); err != nil {
		logger.Error("Alas, there's been an error:", "message", err)
		os.Exit(1)
	}
}
