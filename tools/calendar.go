package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/calendar"
)

const timeHelp = "YYYY-MM-DDTHH:MM in the user's timezone, or YYYY-MM-DD for an all-day event"

func CalendarTools(store *calendar.Store) []Tool {
	readable, writable := store.Readable(), store.Writable()
	if len(readable) == 0 {
		return nil
	}
	readEnum, _ := json.Marshal(readable)
	writeEnum, _ := json.Marshal(writable)
	zone := store.Location().String()

	tools := []Tool{
		{
			Name: "calendar_list",
			Description: "List the user's calendar events for a day, a week (Monday to Sunday) or a month. " +
				"Recurring events are expanded into their occurrences. Use it for questions like \"what do I " +
				"have tomorrow\" or \"am I free on Friday\". Times are in " + zone + ".",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"period": {"type": "string", "enum": ["day", "week", "month"], "description": "How much to list. Default day"},
					"date": {"type": "string", "description": "A day inside the period, YYYY-MM-DD. Default today"},
					"calendars": {"type": "array", "items": {"type": "string", "enum": %s}, "description": "Calendars to look in. Default all"}
				}
			}`, readEnum)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Period    string   `json:"period"`
					Date      string   `json:"date"`
					Calendars []string `json:"calendars"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				from, to, err := period(store, args.Period, args.Date)
				if err != nil {
					return "", err
				}
				events, err := store.Between(args.Calendars, from, to)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Events from %s to %s:\n", from.Format("Mon 2006-01-02"), to.AddDate(0, 0, -1).Format("Mon 2006-01-02"))
				if len(events) == 0 {
					b.WriteString("No events.")
				}
				for _, ev := range events {
					b.WriteString("- " + formatEvent(ev) + "\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name: "calendar_search",
			Description: "Search the user's calendar events by words in their title, description or location, " +
				"for questions like \"when is my dentist appointment\". Each event is listed once, at its first " +
				"occurrence in the range, soonest first. Without from and to it searches the next two years; " +
				"pass from to look at past events.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Words that must all appear, case-insensitive"},
					"from": {"type": "string", "description": "Earliest day, YYYY-MM-DD"},
					"to": {"type": "string", "description": "Last day, YYYY-MM-DD"},
					"calendars": {"type": "array", "items": {"type": "string", "enum": %s}, "description": "Calendars to look in. Default all"},
					"limit": {"type": "integer", "description": "Maximum number of events, 1 to 50. Default 20"}
				},
				"required": ["query"]
			}`, readEnum)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query     string   `json:"query"`
					From      string   `json:"from"`
					To        string   `json:"to"`
					Calendars []string `json:"calendars"`
					Limit     int      `json:"limit"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				today := startOfDay(store.Now())
				from, to := today, today.AddDate(2, 0, 1)
				var err error
				if args.From != "" {
					if from, err = day(store, args.From); err != nil {
						return "", err
					}
				}
				if args.To != "" {
					if to, err = day(store, args.To); err != nil {
						return "", err
					}
					to = to.AddDate(0, 0, 1)
				}
				limit := args.Limit
				if limit <= 0 {
					limit = 20
				}
				events, err := store.Search(args.Calendars, args.Query, from, to, min(limit, 50))
				if err != nil {
					return "", err
				}
				if len(events) == 0 {
					return "No matching events.", nil
				}
				var b strings.Builder
				for _, ev := range events {
					b.WriteString("- " + formatEvent(ev) + "\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:        "calendar_get",
			Description: "Show everything about one calendar event, including its description, by its calendar and uid.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"calendar": {"type": "string", "enum": %s},
					"uid": {"type": "string", "description": "The event uid, from calendar_list or calendar_search"}
				},
				"required": ["calendar", "uid"]
			}`, readEnum)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Calendar string `json:"calendar"`
					Uid      string `json:"uid"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				ev, err := store.Get(args.Calendar, args.Uid)
				if err != nil {
					return "", err
				}
				return describeEvent(ev), nil
			},
		},
	}
	if len(writable) == 0 {
		return tools
	}

	return append(tools,
		Tool{
			Name: "calendar_create",
			Description: "Add an event to one of the user's calendars. Without an end, a timed event lasts an " +
				"hour and an all-day event one day. Times are in " + zone + ".",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"calendar": {"type": "string", "enum": %s},
					"summary": {"type": "string", "description": "The event title"},
					"start": {"type": "string", "description": %q},
					"end": {"type": "string", "description": "Same format as start. For all-day events, the last day"},
					"location": {"type": "string"},
					"description": {"type": "string"},
					"recurrence": {"type": "string", "description": "An iCalendar RRULE for repeating events, such as FREQ=WEEKLY;BYDAY=MO"}
				},
				"required": ["calendar", "summary", "start"]
			}`, writeEnum, timeHelp)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Calendar string `json:"calendar"`
					changeArgs
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				ev, err := store.Create(args.Calendar, args.changes())
				if err != nil {
					return "", err
				}
				return "Created: " + formatEvent(ev), nil
			},
		},
		Tool{
			Name: "calendar_update",
			Description: "Change an event in one of the user's calendars. Only the fields given change; an empty " +
				"string clears description, location or recurrence. Moving the start without an end keeps the " +
				"event's length. On a recurring event the change applies to the whole series.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"calendar": {"type": "string", "enum": %s},
					"uid": {"type": "string", "description": "The event uid, from calendar_list or calendar_search"},
					"summary": {"type": "string"},
					"start": {"type": "string", "description": %q},
					"end": {"type": "string", "description": "Same format as start. For all-day events, the last day"},
					"location": {"type": "string"},
					"description": {"type": "string"},
					"recurrence": {"type": "string", "description": "An iCalendar RRULE, or an empty string to stop repeating"}
				},
				"required": ["calendar", "uid"]
			}`, writeEnum, timeHelp)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Calendar string `json:"calendar"`
					Uid      string `json:"uid"`
					changeArgs
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				ev, err := store.Update(args.Calendar, args.Uid, args.changes())
				if err != nil {
					return "", err
				}
				return "Updated: " + formatEvent(ev), nil
			},
		},
		Tool{
			Name: "calendar_delete",
			Description: "Delete an event from one of the user's calendars, every occurrence if it repeats. " +
				"Only when the user asks for it.",
			Parameters: json.RawMessage(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"calendar": {"type": "string", "enum": %s},
					"uid": {"type": "string", "description": "The event uid, from calendar_list or calendar_search"}
				},
				"required": ["calendar", "uid"]
			}`, writeEnum)),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Calendar string `json:"calendar"`
					Uid      string `json:"uid"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if err := store.Delete(args.Calendar, args.Uid); err != nil {
					return "", err
				}
				return "Deleted.", nil
			},
		},
	)
}

type changeArgs struct {
	Summary     *string `json:"summary"`
	Start       *string `json:"start"`
	End         *string `json:"end"`
	Location    *string `json:"location"`
	Description *string `json:"description"`
	Recurrence  *string `json:"recurrence"`
}

func (a changeArgs) changes() calendar.Changes {
	return calendar.Changes{
		Summary:     a.Summary,
		Description: a.Description,
		Location:    a.Location,
		Start:       a.Start,
		End:         a.End,
		Recurrence:  a.Recurrence,
	}
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func day(store *calendar.Store, v string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(v), store.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot read date %q, expected YYYY-MM-DD", v)
	}
	return t, nil
}

func period(store *calendar.Store, kind, date string) (time.Time, time.Time, error) {
	d := startOfDay(store.Now())
	if date != "" {
		var err error
		if d, err = day(store, date); err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	switch kind {
	case "", "day":
		return d, d.AddDate(0, 0, 1), nil
	case "week":
		from := d.AddDate(0, 0, -(int(d.Weekday())+6)%7)
		return from, from.AddDate(0, 0, 7), nil
	case "month":
		from := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, d.Location())
		return from, from.AddDate(0, 1, 0), nil
	}
	return time.Time{}, time.Time{}, errors.New("period must be day, week or month")
}

func formatWhen(ev calendar.Event) string {
	if ev.AllDay {
		last := ev.End.AddDate(0, 0, -1)
		if !last.After(ev.Start) {
			return ev.Start.Format("Mon 2006-01-02") + ", all day"
		}
		return ev.Start.Format("Mon 2006-01-02") + " to " + last.Format("Mon 2006-01-02") + ", all day"
	}
	if startOfDay(ev.Start).Equal(startOfDay(ev.End)) || ev.End.Equal(startOfDay(ev.End)) && ev.End.Sub(ev.Start) <= 24*time.Hour {
		return ev.Start.Format("Mon 2006-01-02 15:04") + "-" + ev.End.Format("15:04")
	}
	return ev.Start.Format("Mon 2006-01-02 15:04") + " to " + ev.End.Format("Mon 2006-01-02 15:04")
}

func formatEvent(ev calendar.Event) string {
	s := formatWhen(ev) + " " + oneLine(ev.Summary)
	if ev.Location != "" {
		s += " @ " + oneLine(ev.Location)
	}
	var notes []string
	if ev.Recurrence != "" {
		notes = append(notes, "repeats")
	}
	if ev.Status == "CANCELLED" || ev.Status == "TENTATIVE" {
		notes = append(notes, strings.ToLower(ev.Status))
	}
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, ", ") + ")"
	}
	return s + fmt.Sprintf(" [calendar: %s, uid: %s]", ev.Calendar, ev.Uid)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", ", ")), " ")
}

func describeEvent(ev calendar.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Summary: %s\nWhen: %s\nCalendar: %s\nUid: %s", ev.Summary, formatWhen(ev), ev.Calendar, ev.Uid)
	if ev.Location != "" {
		b.WriteString("\nLocation: " + ev.Location)
	}
	if ev.Recurrence != "" {
		b.WriteString("\nRepeats: " + ev.Recurrence + " (from the first occurrence above)")
	}
	if ev.Status != "" {
		b.WriteString("\nStatus: " + strings.ToLower(ev.Status))
	}
	if ev.Description != "" {
		b.WriteString("\nDescription:\n" + ev.Description)
	}
	return b.String()
}
