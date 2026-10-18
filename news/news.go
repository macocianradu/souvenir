// Package news reads a Miniflux server's feeds and entries through its REST
// API. It only ever sends GET requests.
package news

import (
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

const maxBody = 8 << 20

type Category struct {
	Id    int64  `json:"id"`
	Title string `json:"title"`
}

type Feed struct {
	Id       int64    `json:"id"`
	Title    string   `json:"title"`
	SiteUrl  string   `json:"site_url"`
	Category Category `json:"category"`
}

type Entry struct {
	Id          int64     `json:"id"`
	Title       string    `json:"title"`
	Url         string    `json:"url"`
	Author      string    `json:"author"`
	Content     string    `json:"content"`
	PublishedAt time.Time `json:"published_at"`
	Status      string    `json:"status"`
	Starred     bool      `json:"starred"`
	ReadingTime int       `json:"reading_time"`
	Feed        Feed      `json:"feed"`
}

type Entries struct {
	Total   int     `json:"total"`
	Entries []Entry `json:"entries"`
}

type Query struct {
	Search     string
	Status     string
	Starred    bool
	FeedId     int64
	CategoryId int64
	After      time.Time
	Limit      int
}

type Client struct {
	logger slog.Logger
	url    string
	token  string
	client *http.Client
}

func New(cfg config.NewsConfig) *Client {
	return &Client{
		logger: *slog.Default().With("Component", "News"),
		url:    strings.TrimSuffix(cfg.Url, "/"),
		token:  cfg.Token,
		client: &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Millisecond},
	}
}

func (c *Client) Entries(ctx context.Context, q Query) (Entries, error) {
	params := url.Values{"order": {"published_at"}, "direction": {"desc"}}
	if q.Search != "" {
		params.Set("search", q.Search)
	}
	if q.Status != "" {
		params.Set("status", q.Status)
	}
	if q.Starred {
		params.Set("starred", "true")
	}
	if q.FeedId > 0 {
		params.Set("feed_id", strconv.FormatInt(q.FeedId, 10))
	}
	if q.CategoryId > 0 {
		params.Set("category_id", strconv.FormatInt(q.CategoryId, 10))
	}
	if !q.After.IsZero() {
		params.Set("published_after", strconv.FormatInt(q.After.Unix(), 10))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	var out Entries
	return out, c.get(ctx, "/v1/entries?"+params.Encode(), &out)
}

func (c *Client) Entry(ctx context.Context, id int64) (Entry, error) {
	var out Entry
	return out, c.get(ctx, "/v1/entries/"+strconv.FormatInt(id, 10), &out)
}

func (c *Client) Feeds(ctx context.Context) ([]Feed, error) {
	var out []Feed
	return out, c.get(ctx, "/v1/feeds", &out)
}

func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	var out []Category
	return out, c.get(ctx, "/v1/categories", &out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Auth-Token", c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("Request failed", "error", err)
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("not found")
	default:
		c.logger.Error("Miniflux returned an error", "status", resp.Status, "body", strings.TrimSpace(string(data[:min(len(data), 512)])))
		return fmt.Errorf("miniflux returned %s", resp.Status)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("could not read the miniflux response: %w", err)
	}
	return nil
}
