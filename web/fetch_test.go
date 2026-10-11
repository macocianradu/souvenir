package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
)

var testCfg = config.FetchConfig{Enabled: true, Timeout: 5000, MaxBytes: 1 << 20, MaxChars: 1000}

func TestAllowed_OnlyPublicAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.18.0.5": false, "192.168.129.10": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "224.0.0.1": false,
		"::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

func TestFetch_RefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the internal server")
	}))
	defer srv.Close()
	_, err := NewFetcher(testCfg).Fetch(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("got %v", err)
	}
}

func TestFetch_RefusesOtherSchemes(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://x"} {
		if _, err := NewFetcher(testCfg).Fetch(context.Background(), u); err == nil {
			t.Errorf("%s fetched", u)
		}
	}
}

func serve(contentType, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte(body))
	}))
}

func anyAddress(netip.Addr) bool { return true }

func TestFetch_TurnsAnArticleIntoMarkdown(t *testing.T) {
	srv := serve("text/html; charset=utf-8", `<html><head><title>Big news</title></head><body>
		<nav><a href="/">Home</a> <a href="/about">About</a></nav>
		<article><h1>Big news</h1>
		<p>The first paragraph of the story is long enough to count as content, with <b>bold</b> words and a
		<a href="/more">relative link</a> that should become absolute in the result.</p>
		<p>A second paragraph keeps the readability scoring happy, adding more sentences about the story so
		that the article is clearly the main content of this page and not the navigation.</p></article>
		<footer>Copyright</footer></body></html>`)
	defer srv.Close()

	page, err := newFetcher(testCfg, anyAddress).Fetch(context.Background(), srv.URL+"/story")
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Big news" || page.Url != srv.URL+"/story" {
		t.Errorf("title %q url %q", page.Title, page.Url)
	}
	for _, want := range []string{"**bold**", "[relative link](" + srv.URL + "/more)", "second paragraph"} {
		if !strings.Contains(page.Content, want) {
			t.Errorf("missing %q in %q", want, page.Content)
		}
	}
	if strings.Contains(page.Content, "Copyright") {
		t.Errorf("footer kept: %q", page.Content)
	}
}

func TestFetch_KeepsPlainTextAndRefusesBinaries(t *testing.T) {
	text := serve("text/plain", "  just text  ")
	defer text.Close()
	page, err := newFetcher(testCfg, anyAddress).Fetch(context.Background(), text.URL)
	if err != nil || page.Content != "just text" {
		t.Fatalf("%q %v", page.Content, err)
	}

	image := serve("image/png", "\x89PNG")
	defer image.Close()
	if _, err := newFetcher(testCfg, anyAddress).Fetch(context.Background(), image.URL); err == nil {
		t.Fatal("image accepted")
	}
}
