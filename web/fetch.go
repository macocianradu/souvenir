package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"codeberg.org/readeck/go-readability/v2"
	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"git.estatecloud.org/radumaco/souvenir/config"
)

const maxRedirects = 5

type Page struct {
	Url     string
	Title   string
	Content string
}

type Fetcher struct {
	logger   slog.Logger
	client   *http.Client
	maxBytes int64
}

func NewFetcher(cfg config.FetchConfig) *Fetcher {
	return newFetcher(cfg, allowed)
}

func newFetcher(cfg config.FetchConfig, allow func(netip.Addr) bool) *Fetcher {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			addr, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !allow(addr.Addr()) {
				return fmt.Errorf("%s is not a public address", addr.Addr())
			}
			return nil
		},
	}
	return &Fetcher{
		logger:   *slog.Default().With("Component", "Fetch"),
		maxBytes: int64(cfg.MaxBytes),
		client: &http.Client{
			Timeout: time.Duration(cfg.Timeout) * time.Millisecond,
			// No Proxy: a proxy would make the dial check see only the proxy.
			Transport: &http.Transport{DialContext: dialer.DialContext, ForceAttemptHTTP2: true},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return errors.New("too many redirects")
				}
				return checkScheme(req.URL)
			},
		},
	}
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
}

func allowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

func checkScheme(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http and https URLs can be fetched, not %q", u.Scheme)
	}
	return nil
}

func (f *Fetcher) Fetch(ctx context.Context, rawUrl string) (Page, error) {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return Page{}, err
	}
	if err := checkScheme(u); err != nil {
		return Page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; souvenir)")
	resp, err := f.client.Do(req)
	if err != nil {
		f.logger.Warn("Fetch failed", "url", rawUrl, "error", err)
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Page{}, fmt.Errorf("%s returned %s", rawUrl, resp.Status)
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	body, err := charset.NewReader(io.LimitReader(resp.Body, f.maxBytes), contentType)
	if err != nil {
		return Page{}, err
	}
	page := Page{Url: resp.Request.URL.String()}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml" || mediaType == "":
		page.Title, page.Content, err = readable(body, resp.Request.URL)
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/xml" || strings.HasSuffix(mediaType, "+xml"):
		var data []byte
		data, err = io.ReadAll(body)
		page.Content = string(data)
	default:
		return Page{}, fmt.Errorf("cannot read %s content", mediaType)
	}
	if err != nil {
		return Page{}, err
	}
	page.Content = strings.TrimSpace(page.Content)
	return page, nil
}

func readable(body io.Reader, pageUrl *url.URL) (string, string, error) {
	doc, err := html.Parse(body)
	if err != nil {
		return "", "", err
	}
	title := ""
	node := doc
	if article, err := readability.FromDocument(doc, pageUrl); err == nil && article.Node != nil {
		title = article.Title()
		node = article.Node
	}
	md, err := htmltomarkdown.ConvertNode(node, converter.WithDomain(pageUrl.Scheme+"://"+pageUrl.Host))
	if err != nil {
		return "", "", err
	}
	return title, string(md), nil
}
