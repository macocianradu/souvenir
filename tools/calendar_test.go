package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/calendar"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/settings"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/model"
)

func TestPeriod_WeeksRunMondayToSunday(t *testing.T) {
	store, err := calendar.New(config.CalendarsConfig{Path: t.TempDir(), Read: []string{"x"}, Timezone: "Europe/Brussels"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, date, from, to string }{
		{"day", "2026-10-11", "2026-10-11", "2026-10-12"},
		{"week", "2026-10-11", "2026-10-05", "2026-10-12"},
		{"week", "2026-10-12", "2026-10-12", "2026-10-19"},
		{"month", "2026-12-31", "2026-12-01", "2027-01-01"},
	} {
		from, to, err := period(store, tc.kind, tc.date)
		if err != nil || from.Format(time.DateOnly) != tc.from || to.Format(time.DateOnly) != tc.to {
			t.Errorf("%s %s: %s to %s, %v", tc.kind, tc.date, from, to, err)
		}
	}
	if _, _, err := period(store, "year", ""); err == nil {
		t.Error("unknown period accepted")
	}
}

func TestFormatEvent_KeepsEachEventOnOneLine(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse("2006-01-02 15:04", s); return v }
	for want, ev := range map[string]calendar.Event{
		"Mon 2026-10-12 23:00-00:00 Gig @ Arena, Brussels [calendar: home, uid: a]": {
			Calendar: "home", Uid: "a", Summary: "Gig", Location: "Arena\nBrussels",
			Start: at("2026-10-12 23:00"), End: at("2026-10-13 00:00"),
		},
		"Thu 2026-10-15 to Sat 2026-10-17, all day Trip (repeats) [calendar: home, uid: b]": {
			Calendar: "home", Uid: "b", Summary: "Trip", AllDay: true, Recurrence: "FREQ=YEARLY",
			Start: at("2026-10-15 00:00"), End: at("2026-10-18 00:00"),
		},
	} {
		if got := formatEvent(ev); got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

func TestCalendarTools_OfferedOnlyToUsersWithACalendarDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := calendar.New(config.CalendarsConfig{Path: root, Write: []string{"assistant"}, Timezone: "Europe/Brussels"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry().Provide(CalendarTools(store))
	create := model.ToolCall{Id: "1", Function: model.FunctionCall{Name: "calendar_create", Arguments: `{"calendar":"assistant","summary":"Dentist","start":"2026-10-20T09:00"}`}}
	dana := users.WithUser(context.Background(), users.User{Id: "1", Name: "dana"})
	evil := users.WithUser(context.Background(), users.User{Id: "2", Name: ".."})

	for name, ctx := range map[string]context.Context{"no user": context.Background(), "dana without a directory": dana, "a user named ..": evil} {
		if specs := r.Specs(ctx); len(specs) != 0 {
			t.Errorf("%s is offered %d calendar tools", name, len(specs))
		}
		if got := r.Call(ctx, create); !strings.Contains(got.Content, "unknown tool") {
			t.Errorf("%s ran the tool: %s", name, got.Content)
		}
	}

	if err := os.Mkdir(filepath.Join(root, "dana"), 0o755); err != nil {
		t.Fatal(err)
	}
	if specs := r.Specs(dana); len(specs) != 6 {
		t.Fatalf("dana is offered %d calendar tools", len(specs))
	}
	if got := r.Call(dana, create); strings.HasPrefix(got.Content, "Error") {
		t.Fatal(got.Content)
	}
	if files, _ := filepath.Glob(filepath.Join(root, "dana", "assistant", "*.ics")); len(files) != 1 {
		t.Fatalf("dana's directory has %d events", len(files))
	}
}

func TestCalendarTools_FollowTheUsersOwnChoiceOfCalendars(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"dana/home", "dana/work", "dana/assistant"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	store, err := calendar.New(config.CalendarsConfig{Path: root, Read: []string{"work"}, Write: []string{"assistant"}, Timezone: "Europe/Brussels"})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry().Provide(CalendarTools(store))
	dana := users.WithUser(context.Background(), users.User{Id: "1", Name: "dana"})
	names := func(ctx context.Context) string {
		var out []string
		for _, spec := range r.Specs(ctx) {
			out = append(out, spec.Function.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(dana); got != "calendar_list,calendar_search,calendar_get,calendar_create,calendar_update,calendar_delete" {
		t.Fatalf("defaults offer %s", got)
	}
	for _, spec := range r.Specs(dana) {
		if !json.Valid(spec.Function.Parameters) {
			t.Fatalf("%s parameters are not valid JSON: %s", spec.Function.Name, spec.Function.Parameters)
		}
	}

	readOnly := settings.WithSettings(dana, settings.Settings{CustomCalendars: true, CalendarRead: []string{"home"}, CalendarWrite: []string{}})
	if got := names(readOnly); got != "calendar_list,calendar_search,calendar_get" {
		t.Fatalf("a read-only choice offers %s", got)
	}
	if params := string(r.Specs(readOnly)[0].Function.Parameters); !strings.Contains(params, `"home"`) || strings.Contains(params, `"work"`) {
		t.Fatalf("the tool does not list the chosen calendars: %s", params)
	}
	create := model.ToolCall{Id: "1", Function: model.FunctionCall{Name: "calendar_create", Arguments: `{"calendar":"assistant","summary":"x","start":"2026-10-20T09:00"}`}}
	if got := r.Call(readOnly, create); !strings.Contains(got.Content, "unknown tool") {
		t.Fatalf("wrote with write access turned off: %s", got.Content)
	}
	list := model.ToolCall{Id: "2", Function: model.FunctionCall{Name: "calendar_list", Arguments: `{"calendars":["work"]}`}}
	if got := r.Call(readOnly, list); !strings.Contains(got.Content, "no read access") {
		t.Fatalf("read a calendar left out of the choice: %s", got.Content)
	}

	none := settings.WithSettings(dana, settings.Settings{CustomCalendars: true})
	if got := names(none); got != "" {
		t.Fatalf("choosing no calendars still offers %s", got)
	}
}
