package calendar

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

var ErrNotFound = errors.New("event not found")

type Event struct {
	Calendar    string
	Uid         string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	AllDay      bool
	Recurrence  string
	Status      string
}

type Changes struct {
	Summary     *string
	Description *string
	Location    *string
	Start       *string
	End         *string
	Recurrence  *string
}

type Store struct {
	root     string
	loc      *time.Location
	readable []string
	writable []string
	now      func() time.Time
}

// New opens the calendars under cfg.Path, in which environment variables
// such as $HOME are expanded. Write access implies read access.
func New(cfg config.CalendarsConfig) (*Store, error) {
	root := os.ExpandEnv(cfg.Path)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("Calendars.Path %q is not a directory", root)
	}
	loc := time.Local
	if cfg.Timezone != "" {
		var err error
		if loc, err = time.LoadLocation(cfg.Timezone); err != nil {
			return nil, err
		}
	}
	readable := append(slices.Clone(cfg.Read), cfg.Write...)
	slices.Sort(readable)
	readable = slices.Compact(readable)
	return &Store{root: root, loc: loc, readable: readable, writable: cfg.Write, now: time.Now}, nil
}

func (s *Store) Location() *time.Location { return s.loc }

func (s *Store) Now() time.Time { return s.now().In(s.loc) }

func (s *Store) Readable() []string { return s.readable }

func (s *Store) Writable() []string { return s.writable }

func (s *Store) checkRead(name string) error {
	if !slices.Contains(s.readable, name) {
		return fmt.Errorf("no read access to calendar %q; readable calendars: %s", name, strings.Join(s.readable, ", "))
	}
	return nil
}

func (s *Store) checkWrite(name string) error {
	if !slices.Contains(s.writable, name) {
		return fmt.Errorf("no write access to calendar %q; writable calendars: %s", name, strings.Join(s.writable, ", "))
	}
	return nil
}

func (s *Store) Between(names []string, from, to time.Time) ([]Event, error) {
	if len(names) == 0 {
		names = s.Readable()
	}
	var out []Event
	for _, name := range names {
		if err := s.checkRead(name); err != nil {
			return nil, err
		}
		files, err := s.load(name)
		if err != nil {
			return nil, err
		}
		for _, cal := range files {
			out = append(out, s.occurrences(name, cal, from, to)...)
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { return a.Start.Compare(b.Start) })
	return out, nil
}

// Search returns the first occurrence in [from, to) of each event whose
// summary, description or location contains every word of the query.
func (s *Store) Search(names []string, query string, from, to time.Time, limit int) ([]Event, error) {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return nil, errors.New("query is required")
	}
	all, err := s.Between(names, from, to)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hits []Event
	for _, ev := range all {
		key := ev.Calendar + "/" + ev.Uid
		text := strings.ToLower(ev.Summary + "\n" + ev.Description + "\n" + ev.Location)
		missing := slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, w) })
		if seen[key] || missing {
			continue
		}
		seen[key] = true
		if hits = append(hits, ev); len(hits) == limit {
			break
		}
	}
	return hits, nil
}

func (s *Store) Get(name, uid string) (Event, error) {
	if err := s.checkRead(name); err != nil {
		return Event{}, err
	}
	_, comp, err := s.find(name, uid)
	if err != nil {
		return Event{}, err
	}
	return s.toEvent(name, comp.Component)
}

func (s *Store) Create(name string, c Changes) (Event, error) {
	if err := s.checkWrite(name); err != nil {
		return Event{}, err
	}
	if c.Summary == nil || c.Start == nil {
		return Event{}, errors.New("summary and start are required")
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//souvenir//EN")
	comp := ical.NewComponent(ical.CompEvent)
	comp.Props.SetText(ical.PropUID, rand.Text())
	comp.Props.SetDateTime(ical.PropCreated, s.now().UTC())
	cal.Children = append(cal.Children, comp)
	if err := s.apply(comp, c); err != nil {
		return Event{}, err
	}
	path := filepath.Join(s.root, name, propValue(comp, ical.PropUID)+".ics")
	if err := write(path, cal); err != nil {
		return Event{}, err
	}
	return s.toEvent(name, comp)
}

// Update changes an event in place; on a recurring event, the whole series.
func (s *Store) Update(name, uid string, c Changes) (Event, error) {
	if err := s.checkWrite(name); err != nil {
		return Event{}, err
	}
	path, comp, err := s.find(name, uid)
	if err != nil {
		return Event{}, err
	}
	if err := s.apply(comp.Component, c); err != nil {
		return Event{}, err
	}
	if comp.parent.Props.Get(ical.PropProductID) == nil {
		comp.parent.Props.SetText(ical.PropProductID, "-//souvenir//EN")
	}
	if err := write(path, comp.parent); err != nil {
		return Event{}, err
	}
	return s.toEvent(name, comp.Component)
}

func (s *Store) Delete(name, uid string) error {
	if err := s.checkWrite(name); err != nil {
		return err
	}
	path, _, err := s.find(name, uid)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Store) load(name string) (map[string]*ical.Calendar, error) {
	paths, err := filepath.Glob(filepath.Join(s.root, name, "*.ics"))
	if err != nil {
		return nil, err
	}
	files := map[string]*ical.Calendar{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		cal, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		s.fixZones(cal)
		files[path] = cal
	}
	return files, nil
}

type located struct {
	*ical.Component
	parent *ical.Calendar
}

// find returns the file holding an event and its main component, the one
// that is not an override of a single occurrence.
func (s *Store) find(name, uid string) (string, located, error) {
	files, err := s.load(name)
	if err != nil {
		return "", located{}, err
	}
	for path, cal := range files {
		for _, comp := range cal.Children {
			if comp.Name == ical.CompEvent && propValue(comp, ical.PropUID) == uid && comp.Props.Get(ical.PropRecurrenceID) == nil {
				return path, located{comp, cal}, nil
			}
		}
	}
	return "", located{}, fmt.Errorf("%w: no event %q in calendar %q", ErrNotFound, uid, name)
}

func propValue(comp *ical.Component, name string) string {
	if p := comp.Props.Get(name); p != nil {
		return p.Value
	}
	return ""
}

func (s *Store) toEvent(name string, comp *ical.Component) (Event, error) {
	ev := ical.Event{Component: comp}
	start, err := ev.DateTimeStart(s.loc)
	if err != nil {
		return Event{}, err
	}
	end, err := ev.DateTimeEnd(s.loc)
	if err != nil {
		return Event{}, err
	}
	out := Event{
		Calendar:   name,
		Uid:        propValue(comp, ical.PropUID),
		Start:      start.In(s.loc),
		End:        end.In(s.loc),
		AllDay:     comp.Props.Get(ical.PropDateTimeStart).ValueType() == ical.ValueDate,
		Recurrence: propValue(comp, ical.PropRecurrenceRule),
	}
	out.Summary, _ = comp.Props.Text(ical.PropSummary)
	out.Description, _ = comp.Props.Text(ical.PropDescription)
	out.Location, _ = comp.Props.Text(ical.PropLocation)
	out.Status, _ = comp.Props.Text(ical.PropStatus)
	if out.End.Before(out.Start) {
		out.End = out.Start
	}
	return out, nil
}

// occurrences expands one file's events into those overlapping [from, to).
// An occurrence moved by a RECURRENCE-ID override shows only as the override.
func (s *Store) occurrences(name string, cal *ical.Calendar, from, to time.Time) []Event {
	moved := map[int64]bool{}
	for _, comp := range cal.Children {
		if t, err := comp.Props.DateTime(ical.PropRecurrenceID, s.loc); comp.Props.Get(ical.PropRecurrenceID) != nil && err == nil {
			moved[t.Unix()] = true
		}
	}
	var out []Event
	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent || comp.Props.Get(ical.PropDateTimeStart) == nil {
			continue
		}
		ev, err := s.toEvent(name, comp)
		if err != nil {
			continue
		}
		set, _ := comp.RecurrenceSet(s.loc)
		if set == nil || comp.Props.Get(ical.PropRecurrenceID) != nil {
			if overlaps(ev, from, to) {
				out = append(out, ev)
			}
			continue
		}
		length := ev.End.Sub(ev.Start)
		for _, start := range set.Between(from.Add(-length), to, true) {
			occ := ev
			occ.Start = start.In(s.loc)
			occ.End = occ.Start.Add(length)
			if ev.AllDay {
				occ.End = occ.Start.AddDate(0, 0, int(length.Round(24*time.Hour)/(24*time.Hour)))
			}
			if !moved[start.Unix()] && overlaps(occ, from, to) {
				out = append(out, occ)
			}
		}
	}
	return out
}

func overlaps(ev Event, from, to time.Time) bool {
	return ev.Start.Before(to) && (ev.End.After(from) || ev.End.Equal(ev.Start) && !ev.Start.Before(from))
}

var offsetName = regexp.MustCompile(`^(?:GMT|UTC)([+-])(\d{2})(\d{2})$`)

// fixZones rewrites times whose TZID Go does not know, such as Apple's
// "GMT+0200", to UTC, so go-ical can read them. Unknown names that are not
// an offset are read in the store's timezone.
func (s *Store) fixZones(cal *ical.Calendar) {
	for _, comp := range cal.Children {
		for _, name := range []string{ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropRecurrenceID, ical.PropExceptionDates} {
			for i := range comp.Props[name] {
				p := &comp.Props[name][i]
				tzid := p.Params.Get(ical.PropTimezoneID)
				if _, err := time.LoadLocation(tzid); tzid == "" || err == nil {
					continue
				}
				loc := s.loc
				if m := offsetName.FindStringSubmatch(tzid); m != nil {
					hours, _ := strconv.Atoi(m[2])
					minutes, _ := strconv.Atoi(m[3])
					offset := hours*3600 + minutes*60
					if m[1] == "-" {
						offset = -offset
					}
					loc = time.FixedZone(tzid, offset)
				}
				if t, err := time.ParseInLocation("20060102T150405", p.Value, loc); err == nil {
					p.Params = ical.Params{}
					p.Value = t.UTC().Format("20060102T150405Z")
				}
			}
		}
	}
}

func (s *Store) parseTime(v string) (t time.Time, allDay bool, err error) {
	if t, err = time.ParseInLocation("2006-01-02", v, s.loc); err == nil {
		return t, true, nil
	}
	if t, err = time.ParseInLocation("2006-01-02T15:04", v, s.loc); err == nil {
		return t, false, nil
	}
	return t, false, fmt.Errorf("cannot read time %q, expected YYYY-MM-DD or YYYY-MM-DDTHH:MM", v)
}

// apply writes changes into an event. Moving the start without an end keeps
// the event's length.
func (s *Store) apply(comp *ical.Component, c Changes) error {
	for name, v := range map[string]*string{ical.PropSummary: c.Summary, ical.PropDescription: c.Description, ical.PropLocation: c.Location} {
		if v != nil && *v != "" {
			comp.Props.SetText(name, *v)
		} else if v != nil {
			comp.Props.Del(name)
		}
	}
	if propValue(comp, ical.PropSummary) == "" {
		return errors.New("summary cannot be empty")
	}
	if c.Recurrence != nil {
		comp.Props.Del(ical.PropRecurrenceRule)
		if *c.Recurrence != "" {
			rule, err := rrule.StrToROption(strings.TrimPrefix(*c.Recurrence, "RRULE:"))
			if err != nil {
				return fmt.Errorf("recurrence: %w", err)
			}
			comp.Props.SetRecurrenceRule(rule)
		}
	}
	if c.Start != nil || c.End != nil {
		if err := s.applyTimes(comp, c.Start, c.End); err != nil {
			return err
		}
	}
	now := s.now().UTC()
	seq, _ := strconv.Atoi(propValue(comp, ical.PropSequence))
	if comp.Props.Get(ical.PropSequence) != nil {
		seq++
	}
	comp.Props.Set(&ical.Prop{Name: ical.PropSequence, Params: ical.Params{}, Value: strconv.Itoa(seq)})
	comp.Props.SetDateTime(ical.PropDateTimeStamp, now)
	comp.Props.SetDateTime(ical.PropLastModified, now)
	return nil
}

func (s *Store) applyTimes(comp *ical.Component, startArg, endArg *string) error {
	var old Event
	if comp.Props.Get(ical.PropDateTimeStart) != nil {
		var err error
		if old, err = s.toEvent("", comp); err != nil {
			return err
		}
	}
	start, allDay := old.Start, old.AllDay
	if startArg != nil {
		var err error
		if start, allDay, err = s.parseTime(*startArg); err != nil {
			return err
		}
	}
	var end time.Time
	switch {
	case endArg != nil:
		var endAllDay bool
		var err error
		if end, endAllDay, err = s.parseTime(*endArg); err != nil {
			return err
		}
		if endAllDay != allDay {
			return errors.New("start and end must both be dates or both be times")
		}
		if allDay {
			end = end.AddDate(0, 0, 1)
		}
	case !old.Start.IsZero() && allDay == old.AllDay:
		end = start.Add(old.End.Sub(old.Start))
	case allDay:
		end = start.AddDate(0, 0, 1)
	default:
		end = start.Add(time.Hour)
	}
	if !end.After(start) {
		return errors.New("end must be after start")
	}
	comp.Props.Del(ical.PropDuration)
	if allDay {
		comp.Props.SetDate(ical.PropDateTimeStart, start)
		comp.Props.SetDate(ical.PropDateTimeEnd, end)
		return nil
	}
	// Without a configured timezone there is no TZID to write.
	loc := s.loc
	if loc == time.Local {
		loc = time.UTC
	}
	comp.Props.SetDateTime(ical.PropDateTimeStart, start.In(loc))
	comp.Props.SetDateTime(ical.PropDateTimeEnd, end.In(loc))
	return nil
}

// write replaces a file atomically, so vdirsyncer never syncs half of one.
func write(path string, cal *ical.Calendar) error {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
