package config

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
)

var ENV_PREFIX = "SOUV__"

type Config struct {
	Api     ApiConfig
	Llm     LLMConfig
	Logging []LogConfig
	Db      DbConfig
}

type DbConfig struct {
	Url      string
	Port     string
	DbName   string
	User     string
	Password string
	History  HistoryConfig
	Memory   MemoryConfig
}

type MemoryConfig struct {
	Enabled bool
}

type HistoryConfig struct {
	Enabled bool
}

type ApiConfig struct {
	Url string
	Key string
}

type LLMConfig struct {
	Model string
}

type LogConfig struct {
	Level  slog.Level
	Format string
	Target string
}

func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, err
			}
		}
	}
	overrideFromEnv(cfg)
	configLogger(*cfg)
	return cfg, cfg.validate()
}

func configLogger(cfg Config) {
	if len(cfg.Logging) < 1 {
		opts := &slog.HandlerOptions{Level: slog.LevelInfo.Level()}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, opts)))
		return
	}
	var handlers []slog.Handler
	for _, c := range cfg.Logging {
		opts := &slog.HandlerOptions{Level: c.Level}
		target, err := getLogTarget(c.Target)
		if err != nil {
			slog.Error("Config error. Could not open target", "target", c.Target)
		}
		switch c.Format {
		case "text":
			handlers = append(handlers, slog.NewTextHandler(target, opts))
		case "json":
			handlers = append(handlers, slog.NewJSONHandler(target, opts))
		}
	}
	slog.SetDefault(slog.New(slog.NewMultiHandler(handlers...)))
}

func getLogTarget(target string) (io.Writer, error) {
	switch target {
	case "", "stdout":
		return os.Stdout, nil
	case "stderr":
		return os.Stderr, nil
	case "discrd":
		return io.Discard, nil
	default:
		return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
}

func defaultConfig() *Config {
	return &Config{
		ApiConfig{
			Url: "",
			Key: ""},
		LLMConfig{Model: ""},
		[]LogConfig{{Level: slog.LevelInfo.Level(), Format: "text"}},
		DbConfig{
			Url:      "localhost",
			User:     "psql",
			Port:     "54321",
			Password: "",
			DbName:   "souvenir",
			History:  HistoryConfig{Enabled: true},
		},
	}
}

func overrideFromEnv(cfg *Config) {
	overrides := []struct {
		conf *string
		key  string
	}{
		{conf: &cfg.Api.Url, key: ENV_PREFIX + "api__url"},
		{conf: &cfg.Api.Key, key: ENV_PREFIX + "api__key"},
		{conf: &cfg.Llm.Model, key: ENV_PREFIX + "llm__model"},
		{conf: &cfg.Db.DbName, key: ENV_PREFIX + "db__dbName"},
		{conf: &cfg.Db.Url, key: ENV_PREFIX + "db__url"},
		{conf: &cfg.Db.User, key: ENV_PREFIX + "db__user"},
		{conf: &cfg.Db.Password, key: ENV_PREFIX + "db__password"},
		{conf: &cfg.Db.Port, key: ENV_PREFIX + "db__port"},
	}
	for _, override := range(overrides) {
		val := os.Getenv(override.key)
		if val != "" {
			*override.conf = val
		}
	}
}

func (cfg Config) validate() error {
	if cfg.Api.Url == "" {
		return errors.New("No API url configured")
	}
	return nil
}
