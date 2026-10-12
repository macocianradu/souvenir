package calendar

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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
	Exceptions  []time.Time
	Status      string
}

type Changes struct {
	Summary     *string
	Description *string
	Location    *string
	Start       *string
	End         *string
	Recurrence  *string
	Exceptions  *[]string
}

type Store struct {
	root     string
	loc      *time.Location
	readable []string
	writable []string
	now      func() time.Time
}

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

func (s *Store) For(user string) (*Store, error) {
	if user == "." || strings.ContainsAny(user, `/\`) || !filepath.IsLocal(user) {
		return nil, fmt.Errorf("the user name %q cannot be a calendar directory", user)
	}
	c := *s
	c.root = filepath.Join(s.root, user)
	return &c, nil
}

func (s *Store) WithAccess(read, write []string) *Store {
	c := *s
	c.readable = append(slices.Clone(read), write...)
	slices.Sort(c.readable)
	c.readable = slices.Compact(c.readable)
	c.writable = slices.Clone(write)
	return &c
}

func (s *Store) Calendars() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

func (s *Store) Exists() bool {
	info, err := os.Stat(s.root)
	return err == nil && info.IsDir()
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
		splitLists(cal)
		s.fixZones(cal)
		files[path] = cal
	}
	return files, nil
}

type located struct {
	*ical.Component
	parent *ical.Calendar
}

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
	out.Exceptions = s.exceptionDays(comp)
	out.Summary, _ = comp.Props.Text(ical.PropSummary)
	out.Description, _ = comp.Props.Text(ical.PropDescription)
	out.Location, _ = comp.Props.Text(ical.PropLocation)
	out.Status, _ = comp.Props.Text(ical.PropStatus)
	if out.End.Before(out.Start) {
		out.End = out.Start
	}
	return out, nil
}

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

func (s *Store) apply(comp *ical.Component, c Changes) error {
	exceptions := s.exceptionDays(comp)
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
	if c.Exceptions != nil {
		exceptions = nil
		for _, v := range *c.Exceptions {
			d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), s.loc)
			if err != nil {
				return fmt.Errorf("cannot read exception %q, expected YYYY-MM-DD", v)
			}
			exceptions = append(exceptions, d)
		}
	}
	if c.Exceptions != nil || c.Recurrence != nil || c.Start != nil || c.End != nil {
		if err := s.setExceptions(comp, exceptions, c.Exceptions != nil); err != nil {
			return err
		}
	}
	if err := s.checkOccurrencesFit(comp); err != nil {
		return err
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

func (s *Store) checkOccurrencesFit(comp *ical.Component) error {
	option, err := comp.Props.RecurrenceRule()
	if err != nil || option == nil || comp.Props.Get(ical.PropDateTimeStart) == nil {
		return err
	}
	ev, err := s.toEvent("", comp)
	if err != nil {
		return err
	}
	option.Dtstart = ev.Start
	rule, err := rrule.NewRRule(*option)
	if err != nil {
		return err
	}
	starts := rule.Between(ev.Start, ev.Start.AddDate(2, 0, 0), true)
	length := ev.End.Sub(ev.Start)
	for i := 1; i < len(starts) && i <= 10; i++ {
		if gap := starts[i].Sub(starts[i-1]); length > gap {
			return fmt.Errorf("each occurrence would last until %s, past the next one on %s: end is the end of "+
				"the first occurrence, not of the series. To end the series, put UNTIL or COUNT in recurrence",
				ev.End.Format("2006-01-02 15:04"), starts[i].Format("2006-01-02 15:04"))
		}
	}
	return nil
}

func (s *Store) exceptionDays(comp *ical.Component) []time.Time {
	var days []time.Time
	for _, p := range comp.Props[ical.PropExceptionDates] {
		if t, err := p.DateTime(s.loc); err == nil {
			t = t.In(s.loc)
			days = append(days, time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, s.loc))
		}
	}
	slices.SortFunc(days, time.Time.Compare)
	return slices.CompactFunc(days, time.Time.Equal)
}

func (s *Store) setExceptions(comp *ical.Component, days []time.Time, strict bool) error {
	comp.Props.Del(ical.PropExceptionDates)
	if len(days) == 0 {
		return nil
	}
	set, err := comp.RecurrenceSet(s.loc)
	if err != nil {
		return err
	}
	if set == nil {
		if strict {
			return errors.New("exceptions need a recurrence")
		}
		return nil
	}
	start, err := comp.Props.DateTime(ical.PropDateTimeStart, s.loc)
	if err != nil {
		return err
	}
	allDay := comp.Props.Get(ical.PropDateTimeStart).ValueType() == ical.ValueDate
	for _, d := range days {
		next := d.AddDate(0, 0, 1)
		var occ *time.Time
		for _, t := range set.Between(d, next, true) {
			if t.Before(next) {
				occ = &t
				break
			}
		}
		if occ == nil {
			if strict {
				return fmt.Errorf("%s is not an occurrence of this event", d.Format("2006-01-02"))
			}
			continue
		}
		p := ical.NewProp(ical.PropExceptionDates)
		if allDay {
			p.SetDate(occ.In(s.loc))
		} else {
			p.SetDateTime(occ.In(start.Location()))
		}
		comp.Props.Add(p)
	}
	return nil
}

func splitLists(cal *ical.Calendar) {
	for _, comp := range cal.Children {
		for _, name := range []string{ical.PropExceptionDates, ical.PropRecurrenceDates} {
			var split []ical.Prop
			for _, p := range comp.Props[name] {
				for _, v := range strings.Split(p.Value, ",") {
					q := p
					q.Params = maps.Clone(p.Params)
					q.Value = strings.TrimSpace(v)
					split = append(split, q)
				}
			}
			if len(split) > 0 {
				comp.Props[name] = split
			}
		}
	}
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
	loc := s.loc
	if loc == time.Local {
		loc = time.UTC
	}
	comp.Props.SetDateTime(ical.PropDateTimeStart, start.In(loc))
	comp.Props.SetDateTime(ical.PropDateTimeEnd, end.In(loc))
	return nil
}

func write(path string, cal *ical.Calendar) error {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
