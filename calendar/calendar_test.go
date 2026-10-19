package calendar

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
)

const weekly = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:standup
DTSTAMP:20261001T000000Z
DTSTART;TZID=Europe/Brussels:20261005T100000
DTEND;TZID=Europe/Brussels:20261005T101500
RRULE:FREQ=WEEKLY;BYDAY=MO
EXDATE;TZID=Europe/Brussels:20261012T100000
EXDATE;TZID=Europe/Brussels:20261019T100000
SUMMARY:Standup
END:VEVENT
BEGIN:VEVENT
UID:standup
DTSTAMP:20261001T000000Z
RECURRENCE-ID;TZID=Europe/Brussels:20261026T100000
DTSTART;TZID=Europe/Brussels:20261027T090000
DTEND;TZID=Europe/Brussels:20261027T091500
SUMMARY:Standup (moved)
END:VEVENT
END:VCALENDAR
`

const outlook = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:flight
DTSTAMP:20261001T000000Z
DTSTART;TZID=GMT+0200:20261007T171000
DTEND;TZID=Romance Standard Time:20261007T184000
SUMMARY:Flight to Turin
DESCRIPTION:Gate opens an hour before
END:VEVENT
END:VCALENDAR
`

const holiday = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:holiday
DTSTAMP:20261001T000000Z
DTSTART;VALUE=DATE:20261008
DTEND;VALUE=DATE:20261010
SUMMARY:Holiday
LOCATION:Turin
END:VEVENT
END:VCALENDAR
`

var brussels, _ = time.LoadLocation("Europe/Brussels")

func newStore(t *testing.T, files map[string]string) *Store {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.ReplaceAll(content, "\n", "\r\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"work", "home", "secret", "assistant"} {
		os.MkdirAll(filepath.Join(root, dir), 0o755)
	}
	s, err := New(config.CalendarsConfig{
		Path:     root,
		Read:     []string{"work", "home"},
		Write:    []string{"assistant"},
		Timezone: "Europe/Brussels",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, brussels) }
	return s
}

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, brussels) }

func summaries(events []Event) string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.Start.Format("01-02 15:04")+" "+ev.Summary)
	}
	return strings.Join(out, "; ")
}

func TestBetween_ExpandsRecurrencesSkippingExceptionsAndOverrides(t *testing.T) {
	s := newStore(t, map[string]string{"work/standup.ics": weekly})
	got, err := s.Between(nil, day(2026, 10, 1), day(2026, 11, 3))
	if err != nil {
		t.Fatal(err)
	}
	// The 12th and 19th are excluded, the 26th moved to the 27th, and the
	// 2nd of November falls after the end of summer time at the same hour.
	want := "10-05 10:00 Standup; 10-27 09:00 Standup (moved); 11-02 10:00 Standup"
	if summaries(got) != want {
		t.Fatalf("got  %s\nwant %s", summaries(got), want)
	}
}

func TestBetween_ReadsUnknownTimezonesAndAllDayEvents(t *testing.T) {
	s := newStore(t, map[string]string{"home/flight.ics": outlook, "home/holiday.ics": holiday})
	got, err := s.Between([]string{"home"}, day(2026, 10, 7), day(2026, 10, 9))
	if err != nil {
		t.Fatal(err)
	}
	// GMT+0200 is read as that offset; an unknown name falls back to the
	// store's timezone.
	if summaries(got) != "10-07 17:10 Flight to Turin; 10-08 00:00 Holiday" {
		t.Fatal(summaries(got))
	}
	if got[0].End.Format("15:04") != "18:40" || !got[1].AllDay || !got[1].End.Equal(day(2026, 10, 10)) {
		t.Fatalf("%+v", got)
	}
}

func TestSearch_MatchesAllWordsOncePerEvent(t *testing.T) {
	s := newStore(t, map[string]string{"work/standup.ics": weekly, "home/flight.ics": outlook, "home/holiday.ics": holiday})
	got, err := s.Search(nil, "turin", day(2026, 1, 1), day(2027, 1, 1), 10)
	if err != nil {
		t.Fatal(err)
	}
	if summaries(got) != "10-07 17:10 Flight to Turin; 10-08 00:00 Holiday" {
		t.Fatal(summaries(got))
	}
	got, _ = s.Search(nil, "STANDUP", day(2026, 10, 20), day(2027, 1, 1), 10)
	if summaries(got) != "10-27 09:00 Standup (moved)" {
		t.Fatal(summaries(got))
	}
	if got, _ := s.Search(nil, "gate hour", day(2026, 1, 1), day(2027, 1, 1), 10); len(got) != 1 {
		t.Fatal(summaries(got))
	}
}

func TestAccess_IsCheckedOnEveryOperation(t *testing.T) {
	s := newStore(t, map[string]string{"secret/x.ics": holiday, "work/standup.ics": weekly})
	summary, start := "x", "2026-10-11"
	checks := map[string]error{
		"list unlisted": func() error { _, err := s.Between([]string{"secret"}, day(2026, 1, 1), day(2027, 1, 1)); return err }(),
		"get unlisted":  func() error { _, err := s.Get("secret", "holiday"); return err }(),
		"create in read-only": func() error {
			_, err := s.Create("work", Changes{Summary: &summary, Start: &start})
			return err
		}(),
		"update read-only": func() error { _, err := s.Update("work", "standup", Changes{Summary: &summary}); return err }(),
		"delete read-only": s.Delete("work", "standup"),
	}
	for name, err := range checks {
		if err == nil || !strings.Contains(err.Error(), "access") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got, _ := s.Between(nil, day(2026, 1, 1), day(2027, 1, 1)); strings.Contains(summaries(got), "Holiday") {
		t.Fatal("an unlisted calendar was read")
	}
	if _, err := s.Get("work", "standup"); err != nil {
		t.Fatal(err)
	}
}

func TestWrite_CreateUpdateDeleteRoundTrip(t *testing.T) {
	s := newStore(t, nil)
	summary, start, end, rule := "Dentist", "2026-10-14T15:00", "2026-10-14T15:45", "FREQ=MONTHLY"
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, End: &end, Recurrence: &rule})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.root, "assistant", ev.Uid+".ics"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DTSTART;TZID=Europe/Brussels:20261014T150000", "SEQUENCE:0"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q in\n%s", want, data)
		}
	}
	if strings.Contains(string(data), "VALUE=TEXT") {
		t.Fatalf("mistyped property in\n%s", data)
	}

	moved, location := "2026-10-16T09:30", "Rue Neuve 1"
	if _, err := s.Update("assistant", ev.Uid, Changes{Start: &moved, Location: &location}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("assistant", ev.Uid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Start.Format("01-02 15:04") != "10-16 09:30" || got.End.Sub(got.Start) != 45*time.Minute ||
		got.Location != location || got.Summary != summary || got.Recurrence != rule {
		t.Fatalf("%+v", got)
	}
	// Write access implies read access.
	if occ, _ := s.Between(nil, day(2026, 11, 1), day(2026, 12, 1)); summaries(occ) != "11-16 09:30 Dentist" {
		t.Fatal(summaries(occ))
	}

	if err := s.Delete("assistant", ev.Uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("assistant", ev.Uid); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestWrite_AllDayEndIsTheLastDay(t *testing.T) {
	s := newStore(t, nil)
	summary, start, end := "Trip", "2026-10-15", "2026-10-17"
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, End: &end})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.AllDay || !ev.End.Equal(day(2026, 10, 18)) {
		t.Fatalf("%+v", ev)
	}
	timed := "2026-10-17T10:00"
	if _, err := s.Update("assistant", ev.Uid, Changes{End: &timed}); err == nil {
		t.Fatal("mixing a date and a time should fail")
	}
	before := "2026-10-14"
	if _, err := s.Update("assistant", ev.Uid, Changes{End: &before}); err == nil {
		t.Fatal("an end before the start should fail")
	}
}

func TestWrite_AcceptsSecondsAndOffsets(t *testing.T) {
	s := newStore(t, nil)
	brussels, _ := time.LoadLocation("Europe/Brussels")
	want := time.Date(2026, 10, 15, 20, 0, 0, 0, brussels)
	for _, start := range []string{"2026-10-15T20:00", "2026-10-15T20:00:00", "2026-10-15 20:00", "2026-10-15T20:00:00+02:00", "2026-10-15T18:00:00Z"} {
		summary := "Match"
		ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start})
		if err != nil {
			t.Fatalf("%s: %v", start, err)
		}
		if ev.AllDay || !ev.Start.Equal(want) {
			t.Errorf("%s: got %v", start, ev.Start)
		}
	}
}

func TestFind_DoesNotEscapeTheCalendar(t *testing.T) {
	s := newStore(t, map[string]string{"secret/holiday.ics": holiday})
	if _, err := s.Get("work", "../secret/holiday"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestNew_ExpandsEnvironmentAndRequiresTheDirectory(t *testing.T) {
	t.Setenv("CALS", t.TempDir())
	if _, err := New(config.CalendarsConfig{Path: "$CALS", Read: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(config.CalendarsConfig{Path: "~/nope", Read: []string{"x"}}); err == nil {
		t.Fatal("a missing directory was accepted")
	}
}

func TestExceptions_SkipOccurrencesUntilTheEnd(t *testing.T) {
	s := newStore(t, nil)
	summary, start, rule := "Course", "2026-10-13T18:00", "FREQ=WEEKLY;BYDAY=TU;UNTIL=20261124T235959Z"
	skip := []string{"2026-11-03", "2026-11-10"}
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, Recurrence: &rule, Exceptions: &skip})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.root, "assistant", ev.Uid+".ics"))
	for _, want := range []string{"EXDATE;TZID=Europe/Brussels:20261103T180000", "EXDATE;TZID=Europe/Brussels:20261110T180000"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q in\n%s", want, data)
		}
	}
	got, _ := s.Between([]string{"assistant"}, day(2026, 10, 1), day(2027, 1, 1))
	want := "10-13 18:00 Course; 10-20 18:00 Course; 10-27 18:00 Course; 11-17 18:00 Course; 11-24 18:00 Course"
	if summaries(got) != want {
		t.Fatalf("got  %s\nwant %s", summaries(got), want)
	}
	if len(ev.Exceptions) != 2 || !ev.Exceptions[0].Equal(day(2026, 11, 3)) {
		t.Fatalf("%v", ev.Exceptions)
	}
}

func TestExceptions_FollowTheSeriesWhenItMoves(t *testing.T) {
	s := newStore(t, nil)
	summary, start, rule, skip := "Course", "2026-10-13T18:00", "FREQ=WEEKLY;BYDAY=TU", []string{"2026-11-03"}
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, Recurrence: &rule, Exceptions: &skip})
	if err != nil {
		t.Fatal(err)
	}
	later := "2026-10-13T19:30"
	if _, err := s.Update("assistant", ev.Uid, Changes{Start: &later}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Between([]string{"assistant"}, day(2026, 10, 27), day(2026, 11, 11))
	if summaries(got) != "10-27 19:30 Course; 11-10 19:30 Course" {
		t.Fatal(summaries(got))
	}

	none := []string{}
	if _, err := s.Update("assistant", ev.Uid, Changes{Exceptions: &none}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Between([]string{"assistant"}, day(2026, 11, 3), day(2026, 11, 4))
	if summaries(got) != "11-03 19:30 Course" {
		t.Fatal("clearing the exceptions did not bring the occurrence back:", summaries(got))
	}
}

func TestExceptions_RejectDaysOutsideTheSeries(t *testing.T) {
	s := newStore(t, nil)
	summary, start, rule := "Course", "2026-10-13T18:00", "FREQ=WEEKLY;BYDAY=TU"
	wednesday := []string{"2026-11-04"}
	if _, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, Recurrence: &rule, Exceptions: &wednesday}); err == nil ||
		!strings.Contains(err.Error(), "not an occurrence") {
		t.Fatalf("got %v", err)
	}
	once := []string{"2026-10-13"}
	if _, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, Exceptions: &once}); err == nil ||
		!strings.Contains(err.Error(), "need a recurrence") {
		t.Fatalf("got %v", err)
	}
}

func TestExceptions_AllDaySeriesUseDates(t *testing.T) {
	s := newStore(t, nil)
	summary, start, rule, skip := "Bins", "2026-10-12", "FREQ=WEEKLY", []string{"2026-10-19"}
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, Recurrence: &rule, Exceptions: &skip})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.root, "assistant", ev.Uid+".ics"))
	if !strings.Contains(string(data), "EXDATE;VALUE=DATE:20261019") {
		t.Fatalf("no date exception in\n%s", data)
	}
	got, _ := s.Between([]string{"assistant"}, day(2026, 10, 12), day(2026, 10, 27))
	if summaries(got) != "10-12 00:00 Bins; 10-26 00:00 Bins" {
		t.Fatal(summaries(got))
	}
}

func TestBetween_ReadsCommaSeparatedExceptions(t *testing.T) {
	listed := strings.Replace(weekly,
		"EXDATE;TZID=Europe/Brussels:20261012T100000\nEXDATE;TZID=Europe/Brussels:20261019T100000",
		"EXDATE;TZID=Europe/Brussels:20261012T100000,20261019T100000", 1)
	s := newStore(t, map[string]string{"work/standup.ics": listed})
	got, err := s.Between(nil, day(2026, 10, 1), day(2026, 11, 3))
	if err != nil {
		t.Fatal(err)
	}
	if summaries(got) != "10-05 10:00 Standup; 10-27 09:00 Standup (moved); 11-02 10:00 Standup" {
		t.Fatal(summaries(got))
	}
}

func TestWrite_RefusesOccurrencesLongerThanTheirRepeat(t *testing.T) {
	s := newStore(t, nil)
	summary, start, end, rule := "French class", "2026-09-22", "2027-06-30", "FREQ=WEEKLY;UNTIL=20270131T235959Z;BYDAY=TU,TH"
	_, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, End: &end, Recurrence: &rule})
	if err == nil || !strings.Contains(err.Error(), "not of the series") {
		t.Fatalf("got %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(s.root, "assistant", "*.ics")); len(files) != 0 {
		t.Fatal("the event was written anyway")
	}

	start, end = "2026-09-22T18:10", "2026-09-22T21:10"
	ev, err := s.Create("assistant", Changes{Summary: &summary, Start: &start, End: &end, Recurrence: &rule})
	if err != nil {
		t.Fatal(err)
	}
	long := "2026-09-25T21:10"
	if _, err := s.Update("assistant", ev.Uid, Changes{End: &long}); err == nil {
		t.Fatal("stretching an occurrence past the next one was accepted")
	}
}

func TestFor_EachUserHasTheirOwnDirectory(t *testing.T) {
	base := newStore(t, map[string]string{"alice/work/a.ics": weekly})
	alice, err := base.For("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := base.For("bob")
	if events, err := alice.Between(nil, day(2026, 10, 1), day(2026, 11, 1)); err != nil || len(events) == 0 {
		t.Fatalf("alice sees %d events: %v", len(events), err)
	}
	if events, err := bob.Between(nil, day(2026, 10, 1), day(2026, 11, 1)); err != nil || len(events) != 0 {
		t.Fatalf("bob, who has no directory yet, sees %d events: %v", len(events), err)
	}
	summary, start := "Dentist", "2026-10-20T09:00"
	ev, err := bob.Create("assistant", Changes{Summary: &summary, Start: &start})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base.root, "bob", "assistant", ev.Uid+".ics")); err != nil {
		t.Fatal("the event is not in bob's directory:", err)
	}
	if _, err := alice.Get("assistant", ev.Uid); err == nil {
		t.Fatal("alice can read bob's event")
	}
	for _, name := range []string{"", ".", "..", "../alice", "a/b", `a\b`, "/abs"} {
		if _, err := base.For(name); err == nil {
			t.Errorf("%q accepted as a user directory", name)
		}
	}
}
