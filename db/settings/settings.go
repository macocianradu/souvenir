// Package settings keeps what each user chose for themselves: which tools
// their assistant may use and which of their calendars it reads and writes.
package settings

import (
	"context"
	"errors"
	"slices"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Settings struct {
	DisabledTools []string
	// CustomCalendars is false until the user picks calendars; until then the
	// configured Read and Write lists apply.
	CustomCalendars bool
	CalendarRead    []string
	CalendarWrite   []string
}

func (s Settings) ToolEnabled(name string) bool {
	return !slices.Contains(s.DisabledTools, name)
}

type contextKey struct{}

// WithSettings carries the user's settings to the tools of a turn.
func WithSettings(ctx context.Context, s Settings) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// FromContext returns the settings in ctx, or the defaults.
func FromContext(ctx context.Context) Settings {
	s, _ := ctx.Value(contextKey{}).(Settings)
	return s
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Get returns the settings of the user in ctx; a user who never saved any
// gets the defaults.
func (st *Store) Get(ctx context.Context) (Settings, error) {
	userId, err := users.Id(ctx)
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	err = st.pool.QueryRow(ctx,
		`
		SELECT disabled_tools, calendar_read IS NOT NULL, COALESCE(calendar_read, '{}'), COALESCE(calendar_write, '{}')
		  FROM user_settings
		 WHERE user_id = $1
		`, userId).Scan(&s.DisabledTools, &s.CustomCalendars, &s.CalendarRead, &s.CalendarWrite)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	return s, err
}

func (st *Store) Save(ctx context.Context, s Settings) error {
	userId, err := users.Id(ctx)
	if err != nil {
		return err
	}
	var read, write []string
	if s.CustomCalendars {
		read, write = nonNil(s.CalendarRead), nonNil(s.CalendarWrite)
	}
	_, err = st.pool.Exec(ctx,
		`
		INSERT INTO user_settings (user_id, disabled_tools, calendar_read, calendar_write)
		     VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE
		        SET disabled_tools = EXCLUDED.disabled_tools,
		            calendar_read  = EXCLUDED.calendar_read,
		            calendar_write = EXCLUDED.calendar_write,
		            updated_at     = now()
		`, userId, nonNil(s.DisabledTools), read, write)
	return err
}

// nonNil keeps an empty choice from being stored as "no choice".
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
