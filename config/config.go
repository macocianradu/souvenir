package config

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
)

const ENV_PREFIX = "SOUV__"

type Config struct {
	Api       ApiConfig
	Llm       LLMConfig
	Logging   []LogConfig
	Db        DbConfig
	Embedding EmbeddingConfig
}

type DbConfig struct {
	Url          string
	Port         string
	DbName       string
	User         string
	Password     string
	History      HistoryConfig
	Memory       MemoryConfig
	ChunkSize    int
	ChunkOverlap int
}

func (config DbConfig) ConnectionString() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(config.User, config.Password),
		Host:   net.JoinHostPort(config.Url, config.Port),
		Path:   config.DbName,
	}
	return u.String()
}

type EmbeddingConfig struct {
	Url       string
	Key       string
	Model     string
	Dim       int
	Timeout   int
	BatchSize int
	Interval  int
}

type MemoryConfig struct {
	Enabled bool
}

type HistoryConfig struct {
	Enabled bool
}

type ApiConfig struct {
	Url     string
	Key     string
	Timeout int
}

type LLMConfig struct {
	Model      string
	TitleModel string
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
	case "discard":
		return io.Discard, nil
	default:
		return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	}
}

func defaultConfig() *Config {
	return &Config{
		ApiConfig{
			Url:     "",
			Key:     "",
			Timeout: 300000,
		},
		LLMConfig{Model: ""},
		[]LogConfig{{Level: slog.LevelInfo.Level(), Format: "text"}},
		DbConfig{
			Url:          "localhost",
			User:         "psql",
			Port:         "54321",
			Password:     "",
			DbName:       "souvenir",
			History:      HistoryConfig{Enabled: true},
			ChunkSize:    400,
			ChunkOverlap: 40,
		},
		EmbeddingConfig{
			Url:       "",
			Key:       "",
			Model:     "",
			BatchSize: 64,
			Timeout:   60000,
			Interval:  60,
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
		{conf: &cfg.Embedding.Url, key: ENV_PREFIX + "embedding__url"},
		{conf: &cfg.Embedding.Key, key: ENV_PREFIX + "embedding__key"},
		{conf: &cfg.Embedding.Model, key: ENV_PREFIX + "embedding__model"},
		{conf: &cfg.Db.DbName, key: ENV_PREFIX + "db__dbName"},
		{conf: &cfg.Db.Url, key: ENV_PREFIX + "db__url"},
		{conf: &cfg.Db.User, key: ENV_PREFIX + "db__user"},
		{conf: &cfg.Db.Password, key: ENV_PREFIX + "db__password"},
		{conf: &cfg.Db.Port, key: ENV_PREFIX + "db__port"},
	}
	for _, override := range overrides {
		val := os.Getenv(override.key)
		if val != "" {
			*override.conf = val
		}
	}
	durationOverrides := []struct {
		conf *int
		key  string
	}{
		{conf: &cfg.Api.Timeout, key: ENV_PREFIX + "api__timeout"},
		{conf: &cfg.Embedding.Timeout, key: ENV_PREFIX + "embedding__timeout"},
		{conf: &cfg.Embedding.Interval, key: ENV_PREFIX + "embedding__interval"},
	}
	for _, override := range durationOverrides {
		val := os.Getenv(override.key)
		if val != "" {
			parsed, err := strconv.Atoi(val)
			if err == nil {
				*override.conf = parsed
			}
		}
	}
}

func (cfg Config) validate() error {
	if cfg.Api.Url == "" {
		return errors.New("No API url configured")
	}
	if cfg.Embedding.Model == "" {
		return errors.New("No embedding model configured")
	}
	if cfg.Embedding.Url == "" {
		return errors.New("No embedding url configured")
	}
	return nil
}
