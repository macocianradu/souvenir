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
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

const ENV_PREFIX = "SOUV__"

type Config struct {
	Api       ApiConfig
	Llm       LLMConfig
	Logging   []LogConfig
	Db        DbConfig
	Embedding EmbeddingConfig
	Ssh       SshConfig
	Calendars CalendarsConfig
	News      NewsConfig
	Monitor   MonitorConfig
	WebSearch WebSearchConfig
	Fetch     FetchConfig
	Web       WebConfig
	Oidc      OidcConfig
}

type WebConfig struct {
	Listen      string
	BaseUrl     string
	SessionDays int
}

type OidcConfig struct {
	Issuer       string
	ClientId     string
	ClientSecret string
	MatchClaim   string
}

type FetchConfig struct {
	Enabled  bool
	Timeout  int
	MaxBytes int
	MaxChars int
}

type WebSearchConfig struct {
	Provider string
	Url      string
	Timeout  int
}

type CalendarsConfig struct {
	Path     string
	Read     []string
	Write    []string
	Timezone string
}

func (c CalendarsConfig) Enabled() bool {
	return c.Path != "" && len(c.Read)+len(c.Write) > 0
}

type NewsConfig struct {
	Url     string
	Token   string
	Users   []string
	Timeout int
}

func (c NewsConfig) Enabled() bool { return c.Url != "" }

type MonitorConfig struct {
	AlertmanagerUrl string
	VmalertUrls     []string
	MetricsUrl      string
	LogsUrl         string
	Users           []string
	Timeout         int
}

func (c MonitorConfig) Enabled() bool {
	return c.AlertmanagerUrl != "" || len(c.VmalertUrls) > 0 || c.MetricsUrl != "" || c.LogsUrl != ""
}

type SshConfig struct {
	Listen         string
	HostKeyPath    string
	AuthorizedKeys string
}

type DbConfig struct {
	Url          string
	Port         string
	DbName       string
	User         string
	Password     string
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
	Url              string
	Key              string
	Model            string
	Dim              int
	Timeout          int
	BatchSize        int
	Interval         int
	Quiet            int
	QueryPrefix      string
	DocPrefix        string
	MaxDistance      float64
	DistanceMargin   float64
	RecallDistance   float64
	RecallDeviations float64
}

type ApiConfig struct {
	Url     string
	Key     string
	Timeout int
}

var ReasoningEfforts = []string{"none", "low", "high", "max"}

type LLMConfig struct {
	Model           string
	TitleModel      string
	Thinking        *bool
	ReasoningEffort string
	MaxToolRounds   int
	MaxTokens       int
	Temperature     *float64
	ContextBudget   int
	KeepRecent      int
	MemoryRecall    int
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
		LLMConfig{MaxToolRounds: 28, MaxTokens: 4096, ContextBudget: 12000, KeepRecent: 8, MemoryRecall: 5},
		[]LogConfig{{Level: slog.LevelInfo, Format: "text"}},
		DbConfig{
			Url:          "localhost",
			User:         "psql",
			Port:         "54321",
			Password:     "",
			DbName:       "souvenir",
			ChunkSize:    400,
			ChunkOverlap: 40,
		},
		EmbeddingConfig{
			Url:              "",
			Key:              "",
			Model:            "",
			BatchSize:        64,
			Timeout:          60000,
			Interval:         60,
			Quiet:            300,
			MaxDistance:      0.72,
			DistanceMargin:   0.15,
			RecallDistance:   0.5,
			RecallDeviations: 3,
		},
		SshConfig{
			Listen:         ":23234",
			HostKeyPath:    ".ssh/souvenir_ed25519",
			AuthorizedKeys: "authorized_keys",
		},
		CalendarsConfig{},
		NewsConfig{Timeout: 10000},
		MonitorConfig{Timeout: 15000},
		WebSearchConfig{Provider: "searxng", Timeout: 10000},
		FetchConfig{Enabled: true, Timeout: 20000, MaxBytes: 5 << 20, MaxChars: 20000},
		WebConfig{SessionDays: 30},
		OidcConfig{MatchClaim: "preferred_username"},
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
	case reflect.Slice:
		if f.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("%s cannot be set from the environment", f.Type())
		}
		var items []string
		for item := range strings.SplitSeq(val, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
		f.Set(reflect.ValueOf(items).Convert(f.Type()))
	default:
		return fmt.Errorf("%s cannot be set from the environment", f.Type())
	}
	return nil
}

func (cfg Config) validate() error {
	if cfg.Api.Url == "" {
		return errors.New("No API url configured")
	}
	if e := cfg.Llm.ReasoningEffort; e != "" && !slices.Contains(ReasoningEfforts, e) {
		return fmt.Errorf("Llm.ReasoningEffort must be one of %s, or left out", strings.Join(ReasoningEfforts, ", "))
	}
	if cfg.Llm.MaxToolRounds <= 0 {
		return errors.New("Llm.MaxToolRounds must be positive")
	}
	if cfg.Llm.MaxTokens < 0 {
		return errors.New("Llm.MaxTokens must not be negative")
	}
	if cfg.Llm.ContextBudget < 0 {
		return errors.New("Llm.ContextBudget must not be negative")
	}
	if cfg.Llm.KeepRecent < 1 {
		return errors.New("Llm.KeepRecent must be at least 1")
	}
	if cfg.Llm.MemoryRecall < 0 {
		return errors.New("Llm.MemoryRecall must not be negative")
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
	if cfg.Embedding.RecallDistance < 0 || cfg.Embedding.RecallDistance > 2 {
		return errors.New("Embedding.RecallDistance must be between 0 and 2")
	}
	if cfg.Embedding.RecallDeviations < 0 {
		return errors.New("Embedding.RecallDeviations must not be negative")
	}
	if cfg.Embedding.DistanceMargin < 0 {
		return errors.New("Embedding.DistanceMargin must not be negative")
	}
	if cfg.Embedding.Quiet < 0 {
		return errors.New("Embedding.Quiet must not be negative")
	}
	if cfg.Db.ChunkOverlap < 0 || cfg.Db.ChunkSize <= cfg.Db.ChunkOverlap {
		return errors.New("Db.ChunkSize must be larger than Db.ChunkOverlap, and the overlap must not be negative")
	}
	if cfg.Fetch.Timeout <= 0 || cfg.Fetch.MaxBytes <= 0 || cfg.Fetch.MaxChars <= 0 {
		return errors.New("Fetch.Timeout, Fetch.MaxBytes and Fetch.MaxChars must be positive")
	}
	if cfg.WebSearch.Timeout <= 0 {
		return errors.New("WebSearch.Timeout must be positive")
	}
	if err := cfg.Web.validate(cfg.Oidc); err != nil {
		return err
	}
	if err := cfg.News.validate(); err != nil {
		return err
	}
	if err := cfg.Monitor.validate(); err != nil {
		return err
	}
	return cfg.Calendars.validate()
}

func (w WebConfig) validate(o OidcConfig) error {
	if w.Listen == "" {
		return nil
	}
	if u, err := url.Parse(w.BaseUrl); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.TrimSuffix(u.Path, "/") != "" {
		return errors.New("Web.BaseUrl must be the frontend's public address, like https://llm.example.com")
	}
	if w.SessionDays <= 0 {
		return errors.New("Web.SessionDays must be positive")
	}
	if o.Issuer == "" || o.ClientId == "" {
		return errors.New("the web frontend signs in with OIDC: set Oidc.Issuer and Oidc.ClientId")
	}
	if o.MatchClaim == "" {
		return errors.New("Oidc.MatchClaim must name a claim, like preferred_username")
	}
	return nil
}

func (c CalendarsConfig) validate() error {
	if c.Path == "" && len(c.Read)+len(c.Write) > 0 {
		return errors.New("Calendars.Read and Calendars.Write need Calendars.Path")
	}
	for _, name := range append(append([]string{}, c.Read...), c.Write...) {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("Calendars: %q is not a calendar directory inside Calendars.Path", name)
		}
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("Calendars.Timezone: %w", err)
		}
	}
	return nil
}

func (c NewsConfig) validate() error {
	if !c.Enabled() {
		return nil
	}
	if err := checkUrl("News.Url", c.Url); err != nil {
		return err
	}
	if c.Token == "" {
		return errors.New("News.Token must be a Miniflux API key")
	}
	if len(c.Users) == 0 {
		return errors.New("News.Users must name the users who may read the feeds")
	}
	if c.Timeout <= 0 {
		return errors.New("News.Timeout must be positive")
	}
	return nil
}

func (c MonitorConfig) validate() error {
	if !c.Enabled() {
		return nil
	}
	for name, u := range map[string]string{"Monitor.AlertmanagerUrl": c.AlertmanagerUrl, "Monitor.MetricsUrl": c.MetricsUrl, "Monitor.LogsUrl": c.LogsUrl} {
		if u == "" {
			continue
		}
		if err := checkUrl(name, u); err != nil {
			return err
		}
	}
	for _, u := range c.VmalertUrls {
		if err := checkUrl("Monitor.VmalertUrls", u); err != nil {
			return err
		}
	}
	if len(c.Users) == 0 {
		return errors.New("Monitor.Users must name the users who may query the monitoring stack")
	}
	if c.Timeout <= 0 {
		return errors.New("Monitor.Timeout must be positive")
	}
	return nil
}

func checkUrl(name, raw string) error {
	if u, err := url.Parse(raw); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s must be an http or https address, like http://miniflux:8080", name)
	}
	return nil
}
