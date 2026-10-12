package webapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/db/users"
)

func TestSettings_TabsAndWhatTheyShow(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	if w := h.get(dana, "/settings"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings/memories" {
		t.Fatalf("/settings answered %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := h.get(dana, "/settings/nope"); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown tab answered %d", w.Code)
	}

	tools := h.get(dana, "/settings/tools").Body.String()
	for _, want := range []string{`href="/settings/tools" class="active" aria-current="page"`, "<strong>Echo</strong>", `name="tool" value="echo" checked`, "<strong>Calendar create</strong>"} {
		if !strings.Contains(tools, want) {
			t.Errorf("the tools tab lacks %s", want)
		}
	}

	cals := h.get(dana, "/settings/calendars").Body.String()
	for _, want := range []string{
		`aria-label="Read work" checked`,
		`aria-label="Write assistant" checked`,
		`aria-label="Read fun">`, // a calendar no configuration names is listed, unticked
		"The ticks are the defaults until you change one.",
	} {
		if !strings.Contains(cals, want) {
			t.Errorf("the calendars tab lacks %s", want)
		}
	}
	if erin := h.get(h.as("erin"), "/settings/calendars").Body.String(); !strings.Contains(erin, "No calendars are set up for you yet") || strings.Contains(erin, `name="read"`) {
		t.Error("a user without calendars is offered calendar choices")
	}
}

func TestSettings_ToolsAndCalendarsSaveAndShapeTheNextTurn(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	if w := h.post(dana, "/settings/tools", url.Values{"tool": {"calendar_list", "calendar_create", "made_up"}}); w.Code != http.StatusNoContent {
		t.Fatalf("saving tools answered %d", w.Code)
	}
	if w := h.post(dana, "/settings/calendars", url.Values{"read": {"fun", "../erin", "missing"}, "write": {"assistant"}}); w.Code != http.StatusNoContent {
		t.Fatalf("saving calendars answered %d", w.Code)
	}
	saved, err := h.settings.Get(users.WithUser(context.Background(), h.user(dana)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.DisabledTools, "echo") || slices.Contains(saved.DisabledTools, "calendar_list") || slices.Contains(saved.DisabledTools, "made_up") {
		t.Errorf("disabled %v", saved.DisabledTools)
	}
	if !saved.CustomCalendars || !slices.Equal(saved.CalendarRead, []string{"fun"}) || !slices.Equal(saved.CalendarWrite, []string{"assistant"}) {
		t.Errorf("calendars %+v", saved)
	}
	if page := h.get(dana, "/settings/calendars").Body.String(); !strings.Contains(page, `aria-label="Read fun" checked`) || strings.Contains(page, "defaults until") {
		t.Error("the calendars tab does not show the saved choice")
	}

	sent := h.post(dana, "/messages", url.Values{"content": {"what is on today?"}})
	h.get(dana, "/turns/"+turnId(t, sent.Body.String())+"/events")
	h.llm.mu.Lock()
	request := h.llm.streamed[len(h.llm.streamed)-1]
	h.llm.mu.Unlock()
	if strings.Contains(request, `"name":"echo"`) || strings.Contains(request, `"name":"calendar_delete"`) {
		t.Error("the model was offered a tool dana turned off")
	}
	if !strings.Contains(request, `"name":"calendar_list"`) || !strings.Contains(request, `"name":"calendar_create"`) {
		t.Error("the model was not offered the tools dana kept")
	}
	if !strings.Contains(request, `"enum":["assistant","fun"]`) || strings.Contains(request, `"work"`) {
		t.Error("the calendar tools do not follow dana's choice of calendars")
	}
}

var memoryIdPattern = regexp.MustCompile(`data-edit="/settings/memories/([^"]+)"`)

func TestSettings_MemoriesCanBeAddedEditedAndForgotten(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	if page := h.get(dana, "/settings/memories").Body.String(); !strings.Contains(page, "0 saved") || !strings.Contains(page, "Nothing remembered yet.") {
		t.Fatal("an empty memory tab is not shown as such")
	}

	w := h.post(dana, "/settings/memories", url.Values{"content": {"  Prefers   trains over buses "}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Prefers trains over buses") || !strings.Contains(w.Body.String(), "1 saved") {
		t.Fatalf("adding answered %d: %s", w.Code, w.Body)
	}
	m := memoryIdPattern.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("no memory to edit")
	}
	id := m[1]
	if w := h.post(dana, "/settings/memories", url.Values{"content": {"Prefers trains over buses"}}); w.Code != http.StatusConflict {
		t.Errorf("adding it again answered %d", w.Code)
	}
	if w := h.post(dana, "/settings/memories", url.Values{"content": {"   "}}); w.Code != http.StatusBadRequest {
		t.Errorf("an empty memory answered %d", w.Code)
	}

	w = h.post(dana, "/settings/memories/"+id, url.Values{"content": {"Prefers trains and walking"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Prefers trains and walking") || strings.Contains(w.Body.String(), "over buses") {
		t.Fatalf("editing answered %d: %s", w.Code, w.Body)
	}

	erin := h.as("erin")
	if w := h.post(erin, "/settings/memories/"+id+"/forget", nil); w.Code != http.StatusNotFound {
		t.Errorf("erin forgot dana's memory: %d", w.Code)
	}
	if w := h.post(erin, "/settings/memories/"+id, url.Values{"content": {"Hijacked"}}); w.Code == http.StatusOK {
		t.Error("erin edited dana's memory")
	}
	if page := h.get(erin, "/settings/memories").Body.String(); strings.Contains(page, "trains") {
		t.Error("erin sees dana's memory")
	}

	w = h.post(dana, "/settings/memories/"+id+"/forget", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "trains") || !strings.Contains(w.Body.String(), "0 saved") {
		t.Fatalf("forgetting answered %d: %s", w.Code, w.Body)
	}
}

func TestSettings_CrossSiteChangesAreRefused(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	for _, path := range []string{"/settings/tools", "/settings/calendars", "/settings/memories"} {
		r := httptest.NewRequest("POST", path, strings.NewReader("content=x"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://evil.test")
		r.AddCookie(dana)
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d", path, w.Code)
		}
	}
	if saved, _ := h.settings.Get(users.WithUser(context.Background(), h.user(dana))); len(saved.DisabledTools) != 0 || saved.CustomCalendars {
		t.Fatal("a cross-site form changed the settings")
	}
}
