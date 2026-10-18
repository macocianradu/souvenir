package tools

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/monitor"
)

func monitorServer(t *testing.T, seen *[]string) config.MonitorConfig {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		now := time.Now().Unix()
		switch r.URL.Path {
		case "/api/v2/alerts":
			w.Write([]byte(`[{"labels":{"alertname":"DiskFull","instance":"walter","severity":"critical"},
				"annotations":{"summary":"Disk / is 97% full"},"startsAt":"2026-10-18T08:00:00Z","status":{"state":"active"}}]`))
		case "/api/v1/rules":
			w.Write([]byte(`{"data":{"groups":[{"name":"host_alerts","rules":[
				{"name":"DiskFull","type":"alerting","query":"disk_used_percent > 95","state":"firing","health":"ok","duration":300,
				 "alerts":[{"state":"firing","value":"97","labels":{"alertname":"DiskFull","instance":"walter"}}]},
				{"name":"HighLoad","type":"alerting","query":"load1 > 8","state":"inactive","health":"ok"},
				{"name":"rate","type":"recording","query":"sum(x)"}]}]}}`))
		case "/api/v1/query":
			fmt.Fprintf(w, `{"data":{"resultType":"vector","result":[{"metric":{"__name__":"up","job":"node"},"value":[%d,"1"]}]}}`, now)
		case "/api/v1/query_range":
			fmt.Fprintf(w, `{"data":{"resultType":"matrix","result":[{"metric":{"job":"node"},"values":[[%d,"1"],[%d,"3"],[%d,"2"]]}]}}`, now-120, now-60, now)
		case "/api/v1/label/__name__/values":
			w.Write([]byte(`{"data":["container_cpu","container_memory"]}`))
		case "/select/logsql/query":
			if strings.Contains(r.URL.Query().Get("query"), "|||") {
				http.Error(w, "cannot parse query: unexpected token", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"_time":"2026-10-18T08:01:02Z","_msg":"connection refused","container":"souvenir","_stream":"{}"}` + "\n" +
				`{"_time":"2026-10-18T08:01:03Z","_msg":"retrying","container":"souvenir"}` + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return config.MonitorConfig{AlertmanagerUrl: srv.URL, VmalertUrls: []string{srv.URL}, MetricsUrl: srv.URL, LogsUrl: srv.URL, Timeout: 1000}
}

func TestMonitorTools_OfferOnlyTheConfiguredBackends(t *testing.T) {
	var seen []string
	cfg := monitorServer(t, &seen)
	if got := len(monitorTools(monitor.New(cfg))); got != 5 {
		t.Fatalf("%d tools", got)
	}
	got := monitorTools(monitor.New(config.MonitorConfig{AlertmanagerUrl: cfg.AlertmanagerUrl, Timeout: 1000}))
	if len(got) != 1 || got[0].Name != "alerts_list" {
		t.Fatalf("%+v", got)
	}
	for _, tool := range monitorTools(monitor.New(cfg)) {
		if !tool.Untrusted || tool.Sensitive {
			t.Fatalf("%s must be untrusted and read-only", tool.Name)
		}
	}
}

func TestMonitorTools_ReadAlertsRulesMetricsAndLogs(t *testing.T) {
	var seen []string
	tools := monitorTools(monitor.New(monitorServer(t, &seen)))
	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"alerts_list", `{}`, []string{"- DiskFull since", `instance="walter"`, "summary: Disk / is 97% full"}},
		{"alert_rules", `{"state":"firing"}`, []string{"DiskFull [group: host_alerts, state: firing]", "query: disk_used_percent > 95", "for: 5m0s", "alert firing, value 97"}},
		{"metrics_query", `{"query":"up"}`, []string{`{__name__="up", job="node"} 1`}},
		{"metrics_query", `{"query":"load","since":"2h"}`, []string{"min 1, max 3, avg 2, last 2"}},
		{"metrics_names", `{"match":"container_.*"}`, []string{"container_cpu\ncontainer_memory"}},
		{"logs_query", `{"query":"error","limit":2}`, []string{`container="souvenir" connection refused`, "Stopped at the limit of 2 lines"}},
		{"logs_query", `{"query":"|||"}`, []string{"cannot parse query"}},
		{"metrics_query", `{"query":"up","since":"90d"}`, []string{"since can be at most 31d"}},
	} {
		got := runTool(t, tools, tc.name, tc.args)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s %s: missing %q in:\n%s", tc.name, tc.args, want, got)
			}
		}
	}
	if got := runTool(t, tools, "alert_rules", `{"name":"load"}`); strings.Contains(got, "DiskFull") || strings.Contains(got, "rate") {
		t.Errorf("filters and recording rules: %s", got)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("sent %s", s)
		}
		if strings.HasPrefix(s, "GET /api/v1/query_range") && !strings.Contains(s, "step=60s") {
			t.Errorf("a 2h range should step by a minute: %s", s)
		}
	}
}

func TestParseAgo(t *testing.T) {
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour, " 1h30m ": 90 * time.Minute} {
		if got, err := parseAgo(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "d", "-1h", "0d", "soon"} {
		if _, err := parseAgo(in); err == nil {
			t.Errorf("%q should not parse", in)
		}
	}
}
