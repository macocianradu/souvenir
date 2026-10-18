package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/monitor"
)

const (
	maxSeries      = 30
	maxRangePoints = 240
	shownPoints    = 24
	maxLogLines    = 200
	logLineChars   = 600
	maxLookback    = 31 * 24 * time.Hour
)

const agoHelp = "How far back, like 30m, 6h or 7d"

func MonitorTools(client *monitor.Client, allowed []string) Provider {
	all := monitorTools(client)
	return Provider{
		Catalog: all,
		Tools: func(ctx context.Context) []Tool {
			if u, ok := users.FromContext(ctx); ok && slices.Contains(allowed, u.Name) {
				return all
			}
			return nil
		},
	}
}

func monitorTools(client *monitor.Client) []Tool {
	var tools []Tool
	if client.HasAlerts() {
		tools = append(tools, Tool{
			Name:      "alerts_list",
			Untrusted: true,
			Description: "List the alerts firing now on the user's server, from Alertmanager, with their labels, " +
				"summary and since when. Start here when the user asks what is wrong or about an alert they got.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"include_silenced": {"type": "boolean", "description": "Also list silenced and inhibited alerts"}
				}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					All bool `json:"include_silenced"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				alerts, err := client.Alerts(ctx, args.All)
				if err != nil {
					return "", err
				}
				if len(alerts) == 0 {
					return "No alerts are firing.", nil
				}
				slices.SortFunc(alerts, func(a, b monitor.Alert) int { return a.StartsAt.Compare(b.StartsAt) })
				var b strings.Builder
				for _, a := range alerts {
					fmt.Fprintf(&b, "- %s since %s", a.Labels["alertname"], a.StartsAt.Local().Format("Mon 2006-01-02 15:04"))
					if a.Status.State != "" && a.Status.State != "active" {
						b.WriteString(" (" + a.Status.State + ")")
					}
					b.WriteString("\n  labels: " + formatLabels(a.Labels, "alertname"))
					for _, k := range slices.Sorted(maps.Keys(a.Annotations)) {
						fmt.Fprintf(&b, "\n  %s: %s", k, truncate(oneLine(a.Annotations[k]), 400))
					}
					b.WriteString("\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		})
	}
	if client.HasRules() {
		tools = append(tools, Tool{
			Name:      "alert_rules",
			Untrusted: true,
			Description: "Show the alerting rules behind the user's alerts, from vmalert: each rule's query, how long " +
				"it must hold, its state and its last error. Use it to explain why an alert fired and which " +
				"metric or log query to look at next.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Only rules whose name contains this, case-insensitive"},
					"state": {"type": "string", "enum": ["firing", "pending", "inactive"], "description": "Only rules in this state"}
				}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name  string `json:"name"`
					State string `json:"state"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				rules, err := client.Rules(ctx)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, r := range rules {
					if r.Type != "" && r.Type != "alerting" {
						continue
					}
					if args.Name != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(args.Name)) {
						continue
					}
					if args.State != "" && r.State != args.State {
						continue
					}
					fmt.Fprintf(&b, "- %s [group: %s, state: %s", r.Name, r.Group, r.State)
					if r.Health != "" && r.Health != "ok" {
						b.WriteString(", health: " + r.Health)
					}
					b.WriteString("]\n  query: " + oneLine(r.Query))
					if r.Duration > 0 {
						fmt.Fprintf(&b, "\n  for: %s", time.Duration(r.Duration*float64(time.Second)))
					}
					if len(r.Labels) > 0 {
						b.WriteString("\n  labels: " + formatLabels(r.Labels))
					}
					if s := r.Annotations["summary"]; s != "" {
						b.WriteString("\n  summary: " + truncate(oneLine(s), 300))
					}
					if r.LastError != "" {
						b.WriteString("\n  last error: " + truncate(oneLine(r.LastError), 300))
					}
					for i, a := range r.Alerts {
						if i == 5 {
							fmt.Fprintf(&b, "\n  and %d more alerts", len(r.Alerts)-i)
							break
						}
						fmt.Fprintf(&b, "\n  alert %s, value %s: %s", a.State, a.Value, formatLabels(a.Labels, "alertname"))
					}
					b.WriteString("\n")
				}
				if b.Len() == 0 {
					return "No matching alerting rules.", nil
				}
				return strings.TrimSpace(b.String()), nil
			},
		})
	}
	if client.HasMetrics() {
		tools = append(tools, Tool{
			Name:      "metrics_names",
			Untrusted: true,
			Description: "List the metric names VictoriaMetrics has, to find what to query with metrics_query. " +
				"Pass a regular expression to narrow them down, such as container_.* or .*memory.*.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"match": {"type": "string", "description": "A regular expression the whole name must match"}}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Match string `json:"match"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				names, err := client.MetricNames(ctx, strings.TrimSpace(args.Match))
				if err != nil {
					return "", err
				}
				if len(names) == 0 {
					return "No matching metrics.", nil
				}
				out := strings.Join(names[:min(len(names), 300)], "\n")
				if len(names) > 300 {
					out += fmt.Sprintf("\n[%d more; narrow the match.]", len(names)-300)
				}
				return out, nil
			},
		}, Tool{
			Name:      "metrics_query",
			Untrusted: true,
			Description: "Run a MetricsQL (PromQL-compatible) query against the user's VictoriaMetrics. Without " +
				"since it gives the current value of each series; with since it summarises each series over that " +
				"time (min, max, average, last) and shows a sample of its points. Aggregate with sum by (...) " +
				"or topk to keep the number of series small.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "The MetricsQL expression"},
					"since": {"type": "string", "description": %q},
					"step": {"type": "string", "description": "Time between points over a range, like 1m or 1h. Default chosen from the range"}
				},
				"required": ["query"]
			}`, agoHelp+". Leave out for the current value")),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query string `json:"query"`
					Since string `json:"since"`
					Step  string `json:"step"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if strings.TrimSpace(args.Query) == "" {
					return "", errors.New("query is required")
				}
				now := time.Now()
				if args.Since == "" {
					series, err := client.Query(ctx, args.Query, now)
					if err != nil {
						return "", err
					}
					return formatSeries(series, false), nil
				}
				ago, err := lookback(args.Since)
				if err != nil {
					return "", err
				}
				step := ago / 120
				if args.Step != "" {
					if step, err = parseAgo(args.Step); err != nil {
						return "", fmt.Errorf("step: %w", err)
					}
				}
				step = max(step, ago/maxRangePoints, time.Second).Round(time.Second)
				series, err := client.QueryRange(ctx, args.Query, now.Add(-ago), now, step)
				if err != nil {
					return "", err
				}
				return formatSeries(series, true), nil
			},
		})
	}
	if client.HasLogs() {
		tools = append(tools, Tool{
			Name:      "logs_query",
			Untrusted: true,
			Description: "Search the server's logs in VictoriaLogs with a LogsQL query. Examples: " +
				"`error` (lines containing the word), `\"connection refused\"` (the phrase), `field:value` and " +
				"`field:~\"regex\"` to match a field, `-word` to exclude, and pipes such as " +
				"`| stats by (field) count()` to count. Run `* | field_names` first to learn the fields there are. " +
				"Keep since short and the query narrow: the output is cut to the limit.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "The LogsQL query, without a _time filter"},
					"since": {"type": "string", "description": %q},
					"limit": {"type": "integer", "description": "Maximum number of lines, 1 to %d. Default 50"}
				},
				"required": ["query"]
			}`, agoHelp+". Default 1h", maxLogLines)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query string `json:"query"`
					Since string `json:"since"`
					Limit int    `json:"limit"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if strings.TrimSpace(args.Query) == "" {
					return "", errors.New("query is required")
				}
				ago := time.Hour
				if args.Since != "" {
					var err error
					if ago, err = lookback(args.Since); err != nil {
						return "", err
					}
				}
				limit := 50
				if args.Limit > 0 {
					limit = min(args.Limit, maxLogLines)
				}
				now := time.Now()
				lines, err := client.Logs(ctx, args.Query, now.Add(-ago), now, limit)
				if err != nil {
					return "", err
				}
				return formatLogs(lines, limit), nil
			},
		})
	}
	return tools
}

func lookback(v string) (time.Duration, error) {
	d, err := parseAgo(v)
	if err != nil {
		return 0, fmt.Errorf("since: %w", err)
	}
	if d > maxLookback {
		return 0, fmt.Errorf("since can be at most %dd", int(maxLookback.Hours()/24))
	}
	return d, nil
}

func parseAgo(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("cannot read %q, expected something like 30m, 6h or 7d", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("cannot read %q, expected something like 30m, 6h or 7d", v)
	}
	return d, nil
}

func formatLabels(labels map[string]string, skip ...string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if !slices.Contains(skip, k) {
			parts = append(parts, fmt.Sprintf("%s=%q", k, truncate(labels[k], 200)))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func formatSeries(series []monitor.Series, overRange bool) string {
	if len(series) == 0 {
		return "No data."
	}
	var b strings.Builder
	for i, s := range series {
		if i == maxSeries {
			fmt.Fprintf(&b, "[%d more series; aggregate or filter the query to see them.]\n", len(series)-i)
			break
		}
		b.WriteString(formatLabels(s.Metric))
		if len(s.Points) == 0 {
			b.WriteString(" no points\n")
			continue
		}
		last := s.Points[len(s.Points)-1]
		if !overRange {
			fmt.Fprintf(&b, " %s\n", formatValue(last.Value))
			continue
		}
		lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
		for _, p := range s.Points {
			lo, hi, sum = min(lo, p.Value), max(hi, p.Value), sum+p.Value
		}
		fmt.Fprintf(&b, "\n  min %s, max %s, avg %s, last %s at %s\n  ", formatValue(lo), formatValue(hi),
			formatValue(sum/float64(len(s.Points))), formatValue(last.Value), last.Time.Local().Format("01-02 15:04"))
		stride := max(1, (len(s.Points)+shownPoints-1)/shownPoints)
		var shown []string
		for j := 0; j < len(s.Points); j += stride {
			p := s.Points[j]
			shown = append(shown, p.Time.Local().Format("01-02 15:04")+" "+formatValue(p.Value))
		}
		b.WriteString(strings.Join(shown, ", ") + "\n")
	}
	return strings.TrimSpace(b.String())
}

func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

func formatLogs(lines []monitor.Log, limit int) string {
	if len(lines) == 0 {
		return "No matching log lines."
	}
	var b strings.Builder
	for _, l := range lines {
		var parts []string
		if t := l["_time"]; t != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
				t = parsed.Local().Format("01-02 15:04:05")
			}
			parts = append(parts, t)
		}
		for _, k := range slices.Sorted(maps.Keys(l)) {
			if k == "_time" || k == "_msg" || k == "_stream" || k == "_stream_id" || l[k] == "" {
				continue
			}
			parts = append(parts, k+"="+strconv.Quote(truncate(l[k], 120)))
		}
		if msg := l["_msg"]; msg != "" {
			parts = append(parts, oneLine(msg))
		}
		b.WriteString(truncate(strings.Join(parts, " "), logLineChars) + "\n")
	}
	if len(lines) == limit {
		fmt.Fprintf(&b, "[Stopped at the limit of %d lines; narrow the query or shorten since for more.]", limit)
	}
	return strings.TrimSpace(b.String())
}
