package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/news"
)

// minifluxServer answers like Miniflux and records each request's method,
// path and query.
func minifluxServer(t *testing.T, seen *[]string) *news.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("X-Auth-Token") != "secret" {
			http.Error(w, `{"error_message":"denied"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/categories":
			w.Write([]byte(`[{"id":3,"title":"Tech"},{"id":4,"title":"World"}]`))
		case "/v1/feeds":
			w.Write([]byte(`[{"id":7,"title":"LWN","category":{"id":3,"title":"Tech"}},{"id":8,"title":"BBC","category":{"id":4,"title":"World"}}]`))
		case "/v1/entries":
			w.Write([]byte(`{"total":12,"entries":[{"id":42,"title":"Kernel 7.3 released","url":"https://lwn.net/a","status":"unread",
				"published_at":"2026-10-17T09:00:00Z","content":"<p>The <b>new</b> kernel is out.</p>","feed":{"id":7,"title":"LWN"}}]}`))
		case "/v1/entries/42":
			w.Write([]byte(`{"id":42,"title":"Kernel 7.3 released","url":"https://lwn.net/a","starred":true,
				"published_at":"2026-10-17T09:00:00Z","content":"<h1>Heading</h1><p>Body text.</p>","feed":{"id":7,"title":"LWN"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return news.New(config.NewsConfig{Url: srv.URL + "/", Token: "secret", Timeout: 1000})
}

func runTool(t *testing.T, tools []Tool, name, args string) string {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			out, err := tool.Run(context.Background(), []byte(args))
			if err != nil {
				return "Error: " + err.Error()
			}
			return out
		}
	}
	t.Fatalf("no tool %q", name)
	return ""
}

func TestNewsTools_OnlyForTheNamedUsers(t *testing.T) {
	var seen []string
	p := NewsTools(minifluxServer(t, &seen), []string{"radu"})
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "1", Name: "radu"})); len(got) != 3 {
		t.Fatalf("radu gets %d tools", len(got))
	}
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "2", Name: "guest"})); got != nil {
		t.Fatal("another user is offered the news tools")
	}
	if got := p.Tools(context.Background()); got != nil {
		t.Fatal("a request without a user is offered the news tools")
	}
	for _, tool := range p.Catalog {
		if !tool.Untrusted {
			t.Fatalf("%s returns feed text, so it must be untrusted", tool.Name)
		}
	}
}

func TestNewsList_ResolvesTheCategoryAndShowsExcerpts(t *testing.T) {
	var seen []string
	out := runTool(t, newsTools(minifluxServer(t, &seen)), "news_list", `{"category":"tech","status":"unread","limit":500}`)
	for _, want := range []string{"1 of 12 matching entries", "Kernel 7.3 released [entry_id: 42]", "LWN", "(unread)", "https://lwn.net/a", "The **new** kernel is out."} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	last := seen[len(seen)-1]
	for _, want := range []string{"GET /v1/entries?", "category_id=3", "status=unread", "limit=50", "direction=desc"} {
		if !strings.Contains(last, want) {
			t.Fatalf("request %q lacks %q", last, want)
		}
	}
	if got := runTool(t, newsTools(minifluxServer(t, &seen)), "news_list", `{"category":"sports"}`); !strings.Contains(got, "categories: Tech, World") {
		t.Fatal(got)
	}
}

func TestNewsGetAndFeeds(t *testing.T) {
	var seen []string
	tools := newsTools(minifluxServer(t, &seen))
	if got := runTool(t, tools, "news_get", `{"entry_id":42}`); !strings.Contains(got, "# Heading") || !strings.Contains(got, "(starred)") {
		t.Fatal(got)
	}
	if got := runTool(t, tools, "news_get", `{"entry_id":9}`); got != "Error: not found" {
		t.Fatal(got)
	}
	got := runTool(t, tools, "news_feeds", `{}`)
	if !strings.Contains(got, "Tech:\n- LWN [feed_id: 7]") || !strings.Contains(got, "World:\n- BBC [feed_id: 8]") {
		t.Fatal(got)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("sent %s", s)
		}
	}
}
