package webapp

import (
	"net/http"
	"slices"
	"time"

	"git.estatecloud.org/radumaco/souvenir/calendar"
	"git.estatecloud.org/radumaco/souvenir/db/users"
)

type toolChoice struct {
	Name    string
	Summary string
	Enabled bool
}

type calendarChoice struct {
	Name  string
	Read  bool
	Write bool
}

type settingsPage struct {
	User             users.User
	Initials         string
	Sidebar          sidebarView
	Tools            []toolChoice
	CalendarsEnabled bool
	CustomCalendars  bool
	Calendars        []calendarChoice
	Saved            bool
}

// userCalendars lists the calendar directories the user has, or none when
// calendars are off or the user has no directory.
func (s *Server) userCalendars(u users.User) ([]string, *calendar.Store) {
	if s.app.Calendars == nil {
		return nil, nil
	}
	store, err := s.app.Calendars.For(u.Name)
	if err != nil {
		return nil, nil
	}
	names, err := store.Calendars()
	if err != nil {
		s.logger.Error("Could not list calendars", "user", u.Name, "error", err)
	}
	return names, store
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	chosen, err := s.app.Settings.Get(ctx)
	if err != nil {
		s.logger.Error("Could not read settings", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not read your settings.", Home: true})
		return
	}
	list, err := s.app.History.GetConversations(ctx)
	if err != nil {
		s.logger.Error("Could not list conversations", "error", err)
	}
	p := settingsPage{
		User:             u,
		Initials:         initials(u),
		Sidebar:          sidebar(list, "", time.Now().In(s.location)),
		CalendarsEnabled: s.app.Calendars != nil,
		CustomCalendars:  chosen.CustomCalendars,
		Saved:            r.URL.Query().Has("saved"),
	}
	for _, t := range s.catalog() {
		p.Tools = append(p.Tools, toolChoice{Name: t.name, Summary: t.summary, Enabled: chosen.ToolEnabled(t.name)})
	}
	names, store := s.userCalendars(u)
	if store != nil {
		read, write := store.Readable(), store.Writable()
		if chosen.CustomCalendars {
			read, write = chosen.CalendarRead, chosen.CalendarWrite
		}
		for _, name := range names {
			p.Calendars = append(p.Calendars, calendarChoice{
				Name:  name,
				Read:  slices.Contains(read, name) || slices.Contains(write, name),
				Write: slices.Contains(write, name),
			})
		}
	}
	s.render(w, http.StatusOK, "settings", p)
}

type catalogEntry struct{ name, summary string }

// catalog is every tool a user could be offered, once per name.
func (s *Server) catalog() []catalogEntry {
	var out []catalogEntry
	for _, t := range s.app.Tools.Catalog() {
		if !slices.ContainsFunc(out, func(e catalogEntry) bool { return e.name == t.Name }) {
			out = append(out, catalogEntry{t.Name, t.Summary()})
		}
	}
	return out
}

// saveSettings keeps only tools and calendars that exist, so a crafted form
// cannot name others.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	if err := r.ParseForm(); err != nil {
		s.page(w, http.StatusBadRequest, page{Message: "Could not read the form.", Home: true})
		return
	}
	chosen, err := s.app.Settings.Get(ctx)
	if err != nil {
		s.logger.Error("Could not read settings", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not save your settings.", Home: true})
		return
	}
	enabled := r.Form["tool"]
	chosen.DisabledTools = nil
	for _, t := range s.catalog() {
		if !slices.Contains(enabled, t.name) {
			chosen.DisabledTools = append(chosen.DisabledTools, t.name)
		}
	}
	if r.Form.Has("calendars") {
		names, _ := s.userCalendars(u)
		keep := func(picked []string) []string {
			out := []string{}
			for _, name := range names {
				if slices.Contains(picked, name) {
					out = append(out, name)
				}
			}
			return out
		}
		chosen.CustomCalendars = true
		chosen.CalendarRead, chosen.CalendarWrite = keep(r.Form["read"]), keep(r.Form["write"])
	}
	if err := s.app.Settings.Save(ctx, chosen); err != nil {
		s.logger.Error("Could not save settings", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not save your settings.", Home: true})
		return
	}
	http.Redirect(w, r, "/settings?saved", http.StatusSeeOther)
}
