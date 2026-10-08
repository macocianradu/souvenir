package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
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
	Url         string
	Key         string
	Model       string
	Dim         int
	Timeout     int
	BatchSize   int
	Interval    int
	Quiet       int
	QueryPrefix string
	DocPrefix   string
	MaxDistance float64
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
	Thinking   *bool
}

type LogConfig struct {
	Level  slog.Level
	Format string
	Target string
}

const defaultLogFile = "souvenir.log"

func Load(path string) (*Config, error) {
	cfg := defaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		case !errors.Is(err, fs.ErrNotExist):
			return nil, err
		}
	}
	if err := overrideFromEnv(cfg); err != nil {
		return nil, err
	}
	if err := configLogger(*cfg); err != nil {
		return nil, err
	}
	return cfg, cfg.validate()
}

// configLogger installs one handler per Logging entry. The TUI owns stdout,
// so an entry without a Target logs to souvenir.log; an empty Logging list
// discards everything.
func configLogger(cfg Config) error {
	var handlers []slog.Handler
	for _, c := range cfg.Logging {
		target, err := getLogTarget(c.Target)
		if err != nil {
			return fmt.Errorf("Could not open log target %q: %w", c.Target, err)
		}
		opts := &slog.HandlerOptions{Level: c.Level}
		switch c.Format {
		case "", "text":
			handlers = append(handlers, slog.NewTextHandler(target, opts))
		case "json":
			handlers = append(handlers, slog.NewJSONHandler(target, opts))
		default:
			return fmt.Errorf("Unknown log format %q, expected text or json", c.Format)
		}
	}
	slog.SetDefault(slog.New(slog.NewMultiHandler(handlers...)))
	return nil
}

func getLogTarget(target string) (io.Writer, error) {
	switch target {
	case "":
		target = defaultLogFile
	case "stdout":
		return os.Stdout, nil
	case "stderr":
		return os.Stderr, nil
	case "discard":
		return io.Discard, nil
	}
	return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func defaultConfig() *Config {
	return &Config{
		ApiConfig{
			Url:     "",
			Key:     "",
			Timeout: 300000,
		},
		LLMConfig{Model: ""},
		[]LogConfig{{Level: slog.LevelInfo, Format: "text"}},
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
			Url:         "",
			Key:         "",
			Model:       "",
			BatchSize:   64,
			Timeout:     60000,
			Interval:    60,
			Quiet:       300,
			MaxDistance: 0.72,
		},
	}
}

func overrideFromEnv(cfg *Config) error {
	for _, kv := range os.Environ() {
		key, val, _ := strings.Cut(kv, "=")
		name, ok := strings.CutPrefix(key, ENV_PREFIX)
		if !ok || val == "" {
			continue
		}
		field, err := lookupField(reflect.ValueOf(cfg).Elem(), strings.Split(name, "__"))
		if err == nil {
			err = setField(field, val)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

func lookupField(v reflect.Value, path []string) (reflect.Value, error) {
	for _, name := range path {
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, fmt.Errorf("%q is not a section", name)
		}
		v = v.FieldByNameFunc(func(field string) bool { return strings.EqualFold(field, name) })
		if !v.IsValid() {
			return reflect.Value{}, fmt.Errorf("unknown config key %q", name)
		}
	}
	return v, nil
}

func setField(f reflect.Value, val string) error {
	if f.Kind() == reflect.Pointer {
		p := reflect.New(f.Type().Elem())
		if err := setField(p.Elem(), val); err != nil {
			return err
		}
		f.Set(p)
		return nil
	}
	switch f.Kind() {
	case reflect.String:
		f.SetString(val)
	case reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return err
		}
		f.SetInt(int64(n))
	case reflect.Float64:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return err
		}
		f.SetFloat(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		f.SetBool(b)
	default:
		return fmt.Errorf("%s cannot be set from the environment", f.Type())
	}
	return nil
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
	if cfg.Embedding.Dim <= 0 {
		return errors.New("Embedding.Dim must be set to the embedding model's vector size")
	}
	if cfg.Embedding.BatchSize <= 0 {
		return errors.New("Embedding.BatchSize must be positive")
	}
	if cfg.Embedding.Interval <= 0 {
		return errors.New("Embedding.Interval must be positive")
	}
	if cfg.Embedding.MaxDistance < 0 || cfg.Embedding.MaxDistance > 2 {
		return errors.New("Embedding.MaxDistance must be between 0 and 2")
	}
	if cfg.Embedding.Quiet < 0 {
		return errors.New("Embedding.Quiet must not be negative")
	}
	if cfg.Db.ChunkOverlap < 0 || cfg.Db.ChunkSize <= cfg.Db.ChunkOverlap {
		return errors.New("Db.ChunkSize must be larger than Db.ChunkOverlap, and the overlap must not be negative")
	}
	return nil
}
