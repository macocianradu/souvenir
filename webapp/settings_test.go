package webapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/db/users"
)

func TestSettings_PageShowsToolsAndTheUsersCalendars(t *testing.T) {
	h := newHarness(t)
	page := h.get(h.as("dana"), "/settings").Body.String()
	for _, want := range []string{
		`name="tool" value="echo" checked`,
		`name="tool" value="calendar_create" checked`,
		`name="read" value="work" aria-label="Read work" checked`,
		`name="write" value="assistant" aria-label="Write assistant" checked`,
		"These are the defaults until you save.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for _, unwanted := range []string{`aria-label="Read home" checked`, `aria-label="Write work" checked`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the defaults should not check %s", unwanted)
		}
	}

	erin := h.get(h.as("erin"), "/settings").Body.String()
	if !strings.Contains(erin, "No calendars are set up for you yet") || strings.Contains(erin, `name="read"`) {
		t.Error("a user without calendars is offered calendar choices")
	}
}

func TestSettings_SavedChoicesShapeTheNextTurn(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	form := url.Values{
		"tool":      {"calendar_list", "calendar_create", "made_up"},
		"calendars": {"1"},
		"read":      {"home", "../erin", "missing"},
		"write":     {"assistant"},
	}
	w := h.post(dana, "/settings", form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/settings?saved" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	saved, err := h.settings.Get(users.WithUser(context.Background(), h.user(dana)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.DisabledTools, "echo") || slices.Contains(saved.DisabledTools, "calendar_list") || slices.Contains(saved.DisabledTools, "made_up") {
		t.Errorf("disabled %v", saved.DisabledTools)
	}
	if !saved.CustomCalendars || !slices.Equal(saved.CalendarRead, []string{"home"}) || !slices.Equal(saved.CalendarWrite, []string{"assistant"}) {
		t.Errorf("calendars %+v", saved)
	}
	if !strings.Contains(h.get(dana, "/settings?saved").Body.String(), "Saved.") {
		t.Error("no confirmation after saving")
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
	if !strings.Contains(request, `"enum":["assistant","home"]`) || strings.Contains(request, `"work"`) {
		t.Error("the calendar tools do not follow dana's choice of calendars")
	}
}

func TestSettings_CrossSiteSaveIsRefused(t *testing.T) {
	h := newHarness(t)
	dana := h.as("dana")
	r := httptest.NewRequest("POST", "/settings", strings.NewReader("tool=echo"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.test")
	r.AddCookie(dana)
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Fatalf("answered %d", w.Code)
	}
	if saved, _ := h.settings.Get(users.WithUser(context.Background(), h.user(dana))); len(saved.DisabledTools) != 0 {
		t.Fatal("a cross-site form changed the settings")
	}
}
