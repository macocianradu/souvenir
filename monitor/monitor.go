// Package monitor reads the monitoring stack: active alerts from
// Alertmanager, rules from vmalert, metrics from VictoriaMetrics and logs from
// VictoriaLogs. It only ever sends GET requests to fixed read endpoints.
package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
)

const maxBody = 16 << 20

type Client struct {
	logger       slog.Logger
	alertmanager string
	vmalert      []string
	metrics      string
	logs         string
	client       *http.Client
}

func New(cfg config.MonitorConfig) *Client {
	var vmalert []string
	for _, u := range cfg.VmalertUrls {
		vmalert = append(vmalert, strings.TrimSuffix(u, "/"))
	}
	return &Client{
		logger:       *slog.Default().With("Component", "Monitor"),
		alertmanager: strings.TrimSuffix(cfg.AlertmanagerUrl, "/"),
		vmalert:      vmalert,
		metrics:      strings.TrimSuffix(cfg.MetricsUrl, "/"),
		logs:         strings.TrimSuffix(cfg.LogsUrl, "/"),
		client:       &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Millisecond},
	}
}

func (c *Client) HasAlerts() bool  { return c.alertmanager != "" }
func (c *Client) HasRules() bool   { return len(c.vmalert) > 0 }
func (c *Client) HasMetrics() bool { return c.metrics != "" }
func (c *Client) HasLogs() bool    { return c.logs != "" }

type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	Status      struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

func (c *Client) Alerts(ctx context.Context, all bool) ([]Alert, error) {
	params := url.Values{"active": {"true"}}
	if !all {
		params.Set("silenced", "false")
		params.Set("inhibited", "false")
	}
	data, err := c.get(ctx, c.alertmanager, "/api/v2/alerts", params)
	if err != nil {
		return nil, err
	}
	var out []Alert
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("could not read the alertmanager response: %w", err)
	}
	return out, nil
}

type Rule struct {
	Group       string            `json:"-"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Query       string            `json:"query"`
	State       string            `json:"state"`
	Health      string            `json:"health"`
	LastError   string            `json:"lastError"`
	Duration    float64           `json:"duration"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	Alerts      []struct {
		State  string            `json:"state"`
		Labels map[string]string `json:"labels"`
		Value  string            `json:"value"`
	} `json:"alerts"`
}

func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	var out []Rule
	for _, base := range c.vmalert {
		data, err := c.get(ctx, base, "/api/v1/rules", nil)
		if err != nil {
			return nil, err
		}
		var raw struct {
			Data struct {
				Groups []struct {
					Name  string `json:"name"`
					Rules []Rule `json:"rules"`
				} `json:"groups"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("could not read the vmalert response: %w", err)
		}
		for _, g := range raw.Data.Groups {
			for _, r := range g.Rules {
				r.Group = g.Name
				out = append(out, r)
			}
		}
	}
	return out, nil
}

type Series struct {
	Metric map[string]string
	Points []Point
}

type Point struct {
	Time  time.Time
	Value float64
}

func (c *Client) Query(ctx context.Context, query string, at time.Time) ([]Series, error) {
	params := url.Values{"query": {query}, "time": {unix(at)}}
	return c.series(ctx, "/api/v1/query", params)
}

func (c *Client) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	params := url.Values{"query": {query}, "start": {unix(start)}, "end": {unix(end)}, "step": {strconv.Itoa(int(step.Seconds())) + "s"}}
	return c.series(ctx, "/api/v1/query_range", params)
}

func (c *Client) MetricNames(ctx context.Context, match string) ([]string, error) {
	params := url.Values{}
	if match != "" {
		params.Set("match[]", fmt.Sprintf(`{__name__=~%q}`, match))
	}
	data, err := c.get(ctx, c.metrics, "/api/v1/label/__name__/values", params)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("could not read the metrics response: %w", err)
	}
	return raw.Data, nil
}

func (c *Client) series(ctx context.Context, path string, params url.Values) ([]Series, error) {
	data, err := c.get(ctx, c.metrics, path, params)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("could not read the metrics response: %w", err)
	}
	var out []Series
	for _, r := range raw.Data.Result {
		s := Series{Metric: r.Metric}
		if r.Value != nil {
			r.Values = append(r.Values, r.Value)
		}
		for _, v := range r.Values {
			if p, ok := point(v); ok {
				s.Points = append(s.Points, p)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func point(v []any) (Point, bool) {
	if len(v) != 2 {
		return Point{}, false
	}
	ts, ok := v[0].(float64)
	str, ok2 := v[1].(string)
	if !ok || !ok2 {
		return Point{}, false
	}
	val, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return Point{}, false
	}
	return Point{Time: time.Unix(0, int64(ts*float64(time.Second))), Value: val}, true
}

type Log map[string]string

func (c *Client) Logs(ctx context.Context, query string, start, end time.Time, limit int) ([]Log, error) {
	params := url.Values{"query": {query}, "start": {start.UTC().Format(time.RFC3339)}, "end": {end.UTC().Format(time.RFC3339)}, "limit": {strconv.Itoa(limit)}}
	data, err := c.get(ctx, c.logs, "/select/logsql/query", params)
	if err != nil {
		return nil, err
	}
	var out []Log
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() && len(out) < limit {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var l Log
		if err := json.Unmarshal(line, &l); err != nil {
			return nil, fmt.Errorf("could not read the logs response: %w", err)
		}
		out = append(out, l)
	}
	return out, scanner.Err()
}

func (c *Client) get(ctx context.Context, base, path string, params url.Values) ([]byte, error) {
	target := base + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("Request failed", "path", path, "error", err)
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body := strings.TrimSpace(string(data[:min(len(data), 512)]))
		c.logger.Warn("Monitoring request failed", "path", path, "status", resp.Status, "body", body)
		return nil, fmt.Errorf("%s returned %s: %s", path, resp.Status, body)
	}
	return data, nil
}

func unix(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}
