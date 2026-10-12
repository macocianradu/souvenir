package webapp

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

var staticVersions = func() map[string]string {
	versions := map[string]string{}
	fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := staticFiles.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		versions[strings.TrimPrefix(path, "static/")] = hex.EncodeToString(sum[:4])
		return nil
	})
	return versions
}()

func staticURL(name string) string {
	return "/static/" + name + "?v=" + staticVersions[name]
}

const (
	lightBackground = "#e5e9f0"
	darkBackground  = "#23272f"
)

func manifest(w http.ResponseWriter, r *http.Request) {
	type icon struct {
		Src     string `json:"src"`
		Sizes   string `json:"sizes"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
	}
	var icons []icon
	for _, purpose := range []string{"any", "maskable"} {
		for _, size := range []string{"192", "512"} {
			icons = append(icons, icon{Src: staticURL("icon-" + size + ".png"), Sizes: size + "x" + size, Type: "image/png", Purpose: purpose})
		}
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":               "/",
		"name":             "Souvenir",
		"short_name":       "Souvenir",
		"description":      "Your assistant, with a memory.",
		"start_url":        "/",
		"scope":            "/",
		"display":          "standalone",
		"background_color": lightBackground,
		"theme_color":      lightBackground,
		"icons":            icons,
	})
}

func staticHandler() http.Handler {
	sub, _ := fs.Sub(staticFiles, "static")
	files := http.FileServerFS(sub)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	}))
}

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"static": staticURL,
}).ParseFS(templateFiles, "templates/*.html"))

var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

func renderMarkdown(text string) template.HTML {
	var b bytes.Buffer
	if err := markdown.Convert([]byte(text), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	return template.HTML(b.String())
}

type messageView struct {
	Role    string
	Content string
	HTML    template.HTML
	Images  []template.URL
	Time    stamp
}

type stamp struct {
	ISO   string
	Short string
	Full  string
}

func newStamp(at, now time.Time) stamp {
	if at.IsZero() {
		return stamp{}
	}
	loc := now.Location()
	at = at.In(loc)
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, loc)
	clock := at.Format("15:04")
	var short string
	switch {
	case !at.Before(today):
		short = clock
	case !at.Before(today.AddDate(0, 0, -1)):
		short = "Yesterday " + clock
	case !at.Before(today.AddDate(0, 0, -6)):
		short = at.Format("Mon") + " " + clock
	case at.Year() == now.Year():
		short = at.Format("2 Jan") + " " + clock
	default:
		short = at.Format("2 Jan 2006") + " " + clock
	}
	return stamp{ISO: at.UTC().Format(time.RFC3339), Short: short, Full: at.Format("Monday 2 January 2006, 15:04")}
}

var imageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

func messageViews(messages []model.Message, loc *time.Location) []messageView {
	now := time.Now().In(loc)
	var views []messageView
	for _, m := range messages {
		switch m.Role {
		case "user":
			v := messageView{Role: m.Role, Content: m.Content, Time: newStamp(m.CreatedAt, now)}
			for _, img := range m.Images {
				if slices.Contains(imageTypes, img.MediaType) {
					v.Images = append(v.Images, template.URL(img.DataURL()))
				}
			}
			views = append(views, v)
		case "assistant":
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			views = append(views, messageView{Role: m.Role, Content: m.Content, HTML: renderMarkdown(m.Content), Time: newStamp(m.CreatedAt, now)})
		}
	}
	return views
}

type sidebarItem struct {
	Id     string
	Name   string
	Active bool
}

type sidebarGroup struct {
	Label string
	Items []sidebarItem
}

type sidebarView struct {
	Groups []sidebarGroup
}

func sidebar(conversations []model.Conversation, active string, now time.Time) sidebarView {
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	bounds := []struct {
		label string
		since time.Time
	}{
		{"Today", today},
		{"Yesterday", today.AddDate(0, 0, -1)},
		{"Previous 7 days", today.AddDate(0, 0, -7)},
		{"Previous 30 days", today.AddDate(0, 0, -30)},
		{"Older", time.Time{}},
	}
	var view sidebarView
	for _, c := range conversations {
		label := bounds[len(bounds)-1].label
		for _, b := range bounds {
			if !c.UpdatedAt.Before(b.since) {
				label = b.label
				break
			}
		}
		if n := len(view.Groups); n == 0 || view.Groups[n-1].Label != label {
			view.Groups = append(view.Groups, sidebarGroup{Label: label})
		}
		name := c.Title
		if name == "" {
			name = oneLine(c.Preview, 60)
		}
		if name == "" {
			name = "Untitled"
		}
		g := &view.Groups[len(view.Groups)-1]
		g.Items = append(g.Items, sidebarItem{Id: c.Id, Name: name, Active: c.Id == active})
	}
	return view
}

func oneLine(s string, n int) string {
	runes := []rune(strings.Join(strings.Fields(s), " "))
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n-1]) + "…"
}

func initials(u users.User) string {
	var out []rune
	for _, word := range strings.FieldsFunc(u.Name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		out = append(out, unicode.ToUpper([]rune(word)[0]))
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}
