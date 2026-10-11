package tools

import (
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/calendar"
	"git.estatecloud.org/radumaco/souvenir/config"
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
