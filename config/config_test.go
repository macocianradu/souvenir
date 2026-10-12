package config

import (
	"os"
	"strings"
	"testing"
)

func setRequired(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("SOUV__api__url", "http://llm")
	t.Setenv("SOUV__embedding__url", "http://embed")
	t.Setenv("SOUV__embedding__model", "m")
	t.Setenv("SOUV__embedding__dim", "1024")
}

func writeConfig(t *testing.T, json string) string {
	t.Helper()
	if err := os.WriteFile("c.json", []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	return "c.json"
}

func TestEnv_OverridesAnyScalarKeyCaseInsensitively(t *testing.T) {
	setRequired(t)
	t.Setenv("SOUV__db__dbName", "legacy")
	t.Setenv("SOUV__LLM__TITLEMODEL", "cheap")
	t.Setenv("SOUV__llm__thinking", "true")
	t.Setenv("SOUV__embedding__maxDistance", "0.5")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Db.DbName != "legacy" || cfg.Llm.TitleModel != "cheap" ||
		cfg.Embedding.Dim != 1024 || cfg.Embedding.MaxDistance != 0.5 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.Llm.Thinking == nil || !*cfg.Llm.Thinking {
		t.Fatal("pointer field not set")
	}
}

func TestEnv_StringListsAreCommaSeparated(t *testing.T) {
	setRequired(t)
	t.Setenv("SOUV__calendars__path", "/cals")
	t.Setenv("SOUV__calendars__read", "work, home,")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Calendars.Read, "|") != "work|home" || !cfg.Calendars.Enabled() {
		t.Fatalf("%+v", cfg.Calendars)
	}
}

func TestEnv_EmptyValueIsIgnored(t *testing.T) {
	setRequired(t)
	t.Setenv("SOUV__db__chunkSize", "")
	cfg, err := Load("")
	if err != nil || cfg.Db.ChunkSize != 400 {
		t.Fatalf("chunk size %d, err %v", cfg.Db.ChunkSize, err)
	}
}

func TestEnv_InvalidKeysAndValuesFailNamingTheVariable(t *testing.T) {
	for name, value := range map[string]string{
		"SOUV__db__chunkSiz":   "1",
		"SOUV__db__chunkSize":  "lots",
		"SOUV__logging__level": "debug",
		"SOUV__db":             "x",
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(name, value)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected an error naming %s, got %v", name, err)
			}
		})
	}
}

func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := Load("absent.json")
	if err != nil || cfg.Llm.MaxToolRounds == 0 || cfg.Embedding.BatchSize != 64 {
		t.Fatalf("defaults not used: %+v %v", cfg, err)
	}
}

func TestLoad_MalformedFileFails(t *testing.T) {
	setRequired(t)
	if _, err := Load(writeConfig(t, `{bad`)); err == nil {
		t.Fatal("expected a JSON error")
	}
}

func TestLogging_EntryWithoutTargetWritesToDefaultFile(t *testing.T) {
	setRequired(t)
	if _, err := Load(writeConfig(t, `{"Logging":[{"Level":"info"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaultLogFile); err != nil {
		t.Fatal("default log file not created")
	}
}

func TestLogging_BadEntriesFail(t *testing.T) {
	for name, json := range map[string]string{
		"unknown format":    `{"Logging":[{"Format":"yaml"}]}`,
		"unopenable target": `{"Logging":[{"Target":"/nonexistent/dir/x.log"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			if _, err := Load(writeConfig(t, json)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLogging_EmptyListDisablesLogging(t *testing.T) {
	setRequired(t)
	if _, err := Load(writeConfig(t, `{"Logging":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_RejectsUnusableSettings(t *testing.T) {
	for name, env := range map[string][2]string{
		"no dimension":           {"SOUV__embedding__dim", "0"},
		"no batch size":          {"SOUV__embedding__batchSize", "0"},
		"no interval":            {"SOUV__embedding__interval", "0"},
		"negative quiet":         {"SOUV__embedding__quiet", "-1"},
		"distance out of range":  {"SOUV__embedding__maxDistance", "3"},
		"negative margin":        {"SOUV__embedding__distanceMargin", "-0.1"},
		"overlap too large":      {"SOUV__db__chunkOverlap", "400"},
		"no tool rounds":         {"SOUV__llm__maxToolRounds", "0"},
		"negative budget":        {"SOUV__llm__contextBudget", "-1"},
		"nothing kept recent":    {"SOUV__llm__keepRecent", "0"},
		"calendars without path": {"SOUV__calendars__read", "work"},
		"unknown timezone":       {"SOUV__calendars__timezone", "Mars/Olympus"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(""); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestValidate_CalendarNamesStayInsidePath(t *testing.T) {
	for _, name := range []string{"../etc", "/abs", ""} {
		setRequired(t)
		if _, err := Load(writeConfig(t, `{"Calendars":{"Path":"/cals","Write":["`+name+`"]}}`)); err == nil {
			t.Fatalf("%q accepted", name)
		}
	}
}

func TestConnectionString_EscapesCredentials(t *testing.T) {
	cfg := DbConfig{Url: "db", Port: "5432", DbName: "souv", User: "me", Password: "p@ss/word"}
	if got := cfg.ConnectionString(); got != "postgres://me:p%40ss%2Fword@db:5432/souv" {
		t.Fatal(got)
	}
}

func TestValidate_WebNeedsABaseUrlAndOidc(t *testing.T) {
	web := map[string]string{
		"SOUV__web__listen":    ":8080",
		"SOUV__web__baseUrl":   "https://llm.example.com",
		"SOUV__oidc__issuer":   "https://auth.example.com/application/o/souvenir/",
		"SOUV__oidc__clientId": "souvenir",
	}
	for name, change := range map[string][2]string{
		"no base url":         {"SOUV__web__baseUrl", ""},
		"base url with path":  {"SOUV__web__baseUrl", "https://example.com/souvenir"},
		"base url not http":   {"SOUV__web__baseUrl", "ftp://example.com"},
		"no issuer":           {"SOUV__oidc__issuer", ""},
		"no client":           {"SOUV__oidc__clientId", ""},
		"no session lifetime": {"SOUV__web__sessionDays", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequired(t)
			for k, v := range web {
				t.Setenv(k, v)
			}
			// An empty env value is ignored, so a removed setting is unset.
			if change[1] == "" {
				os.Unsetenv(change[0])
			} else {
				t.Setenv(change[0], change[1])
			}
			if _, err := Load(""); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	setRequired(t)
	for k, v := range web {
		t.Setenv(k, v)
	}
	if _, err := Load(""); err != nil {
		t.Fatal(err)
	}
}
