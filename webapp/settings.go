package webapp

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/calendar"
	"git.estatecloud.org/radumaco/souvenir/db/settings"
	"git.estatecloud.org/radumaco/souvenir/db/users"
)

const maxMemory = 1000

var settingsTabs = []struct{ Id, Label string }{
	{"memories", "Memories"},
	{"tools", "Tools"},
	{"calendars", "Calendars"},
}

type toolChoice struct {
	Name    string
	Label   string
	Summary string
	Enabled bool
}

type calendarChoice struct {
	Name  string
	Read  bool
	Write bool
}

type memoryView struct {
	Id      string
	Content string
	Time    stamp
}

type memoriesView struct {
	Memories []memoryView
}

type settingsPage struct {
	User     users.User
	Initials string
	Tabs     []struct{ Id, Label string }
	Tab      string
	Title    string

	Memories memoriesView

	Tools []toolChoice

	CalendarsEnabled bool
	CustomCalendars  bool
	Calendars        []calendarChoice
}

func (s *Server) settingsHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/settings/memories", http.StatusSeeOther)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, _ := users.FromContext(ctx)
	tab := r.PathValue("tab")
	i := slices.IndexFunc(settingsTabs, func(t struct{ Id, Label string }) bool { return t.Id == tab })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	p := settingsPage{User: u, Initials: initials(u), Tabs: settingsTabs, Tab: tab, Title: settingsTabs[i].Label}
	chosen, err := s.app.Settings.Get(ctx)
	if err != nil {
		s.logger.Error("Could not read settings", "error", err)
		s.page(w, http.StatusInternalServerError, page{Message: "Could not read your settings.", Home: true})
		return
	}
	switch tab {
	case "memories":
		if p.Memories, err = s.memoriesView(r); err != nil {
			s.logger.Error("Could not list memories", "error", err)
			s.page(w, http.StatusInternalServerError, page{Message: "Could not read your memories.", Home: true})
			return
		}
	case "tools":
		for _, t := range s.catalog() {
			p.Tools = append(p.Tools, toolChoice{Name: t.name, Label: t.label, Summary: t.summary, Enabled: chosen.ToolEnabled(t.name)})
		}
	case "calendars":
		p.CalendarsEnabled = s.app.Calendars != nil
		p.CustomCalendars = chosen.CustomCalendars
		p.Calendars = s.calendarChoices(u, chosen)
	}
	s.render(w, http.StatusOK, "settings", p)
}

type catalogEntry struct{ name, label, summary string }

func (s *Server) catalog() []catalogEntry {
	var out []catalogEntry
	for _, t := range s.app.Tools.Catalog() {
		if !slices.ContainsFunc(out, func(e catalogEntry) bool { return e.name == t.Name }) {
			out = append(out, catalogEntry{t.Name, toolLabel(t.Name), t.Summary()})
		}
	}
	return out
}

func toolLabel(name string) string {
	label := strings.ReplaceAll(name, "_", " ")
	if label == "" {
		return label
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

// saveTools turns off every known tool not ticked; unknown names are
// ignored, so a crafted form cannot add any.
func (s *Server) saveTools(w http.ResponseWriter, r *http.Request) {
	s.updateSettings(w, r, func(chosen *settings.Settings) {
		enabled := r.Form["tool"]
		chosen.DisabledTools = nil
		for _, t := range s.catalog() {
			if !slices.Contains(enabled, t.name) {
				chosen.DisabledTools = append(chosen.DisabledTools, t.name)
			}
		}
	})
}

func (s *Server) calendarChoices(u users.User, chosen settings.Settings) []calendarChoice {
	names, store := s.userCalendars(u)
	if store == nil {
		return nil
	}
	read, write := store.Readable(), store.Writable()
	if chosen.CustomCalendars {
		read, write = chosen.CalendarRead, chosen.CalendarWrite
	}
	var out []calendarChoice
	for _, name := range names {
		out = append(out, calendarChoice{
			Name:  name,
			Read:  slices.Contains(read, name) || slices.Contains(write, name),
			Write: slices.Contains(write, name),
		})
	}
	return out
}

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

// saveCalendars keeps only calendars the user has.
func (s *Server) saveCalendars(w http.ResponseWriter, r *http.Request) {
	u, _ := users.FromContext(r.Context())
	s.updateSettings(w, r, func(chosen *settings.Settings) {
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
	})
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request, change func(*settings.Settings)) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, "Could not read the form.")
		return
	}
	chosen, err := s.app.Settings.Get(ctx)
	if err == nil {
		change(&chosen)
		err = s.app.Settings.Save(ctx, chosen)
	}
	if err != nil {
		s.logger.Error("Could not save settings", "error", err)
		fail(w, http.StatusInternalServerError, "Could not save your settings.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) memoriesView(r *http.Request) (memoriesView, error) {
	list, err := s.app.Memories.List(r.Context())
	if err != nil {
		return memoriesView{}, err
	}
	now := time.Now().In(s.location)
	var v memoriesView
	for _, m := range list {
		v.Memories = append(v.Memories, memoryView{Id: m.Id, Content: m.Content, Time: newStamp(m.CreatedAt, now)})
	}
	return v, nil
}

func memoryContent(r *http.Request) (string, error) {
	content := strings.Join(strings.Fields(r.FormValue("content")), " ")
	switch {
	case content == "":
		return "", errors.New("A memory cannot be empty.")
	case len([]rune(content)) > maxMemory:
		return "", fmt.Errorf("A memory can be at most %d characters.", maxMemory)
	}
	return content, nil
}

func (s *Server) renderMemories(w http.ResponseWriter, r *http.Request) {
	v, err := s.memoriesView(r)
	if err != nil {
		s.logger.Error("Could not list memories", "error", err)
		fail(w, http.StatusInternalServerError, "Could not read your memories.")
		return
	}
	s.render(w, http.StatusOK, "memories", v)
}

func (s *Server) addMemory(w http.ResponseWriter, r *http.Request) {
	content, err := memoryContent(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	_, created, err := s.app.Memories.Save(r.Context(), content, "")
	if err != nil {
		s.logger.Error("Could not save a memory", "error", err)
		fail(w, http.StatusInternalServerError, "Could not save the memory.")
		return
	}
	if !created {
		fail(w, http.StatusConflict, "That is already remembered.")
		return
	}
	s.renderMemories(w, r)
}

func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	content, err := memoryContent(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.app.Memories.Update(r.Context(), r.PathValue("id"), content); err != nil {
		fail(w, http.StatusBadRequest, "Could not change the memory: "+err.Error())
		return
	}
	s.renderMemories(w, r)
}

func (s *Server) forgetMemory(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Memories.Forget(r.Context(), r.PathValue("id")); err != nil {
		fail(w, http.StatusNotFound, "That memory does not exist any more.")
		return
	}
	s.renderMemories(w, r)
}
