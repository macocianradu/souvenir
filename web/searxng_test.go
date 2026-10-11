package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
)

func TestSearch_SendsTheQueryAndReadsResults(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.Write([]byte(`{
			"answers": ["42", {"answer": "forty-two"}],
			"results": [{"title": "T", "url": "https://x.test", "content": "C", "publishedDate": "2026-10-01"}]
		}`))
	}))
	defer srv.Close()

	found, err := NewSearxng(config.WebSearchConfig{Url: srv.URL + "/", Timeout: 1000}).Search(context.Background(), "a b", "week")
	if err != nil {
		t.Fatal(err)
	}
	if got != "format=json&q=a+b&time_range=week" {
		t.Errorf("query %q", got)
	}
	if len(found.Answers) != 2 || found.Answers[1] != "forty-two" {
		t.Errorf("answers %v", found.Answers)
	}
	if len(found.Results) != 1 || found.Results[0] != (Result{"T", "https://x.test", "C", "2026-10-01"}) {
		t.Errorf("results %+v", found.Results)
	}
}

func TestSearch_ReportsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "json format disabled", http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := NewSearxng(config.WebSearchConfig{Url: srv.URL, Timeout: 1000}).Search(context.Background(), "q", ""); err == nil {
		t.Fatal("no error for a 403")
	}
}

func TestNew_RejectsAnUnknownProvider(t *testing.T) {
	if _, err := New(config.WebSearchConfig{Provider: "altavista"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if s, err := New(config.WebSearchConfig{Provider: "searxng"}); err != nil || s == nil {
		t.Fatalf("searxng: %v", err)
	}
}
