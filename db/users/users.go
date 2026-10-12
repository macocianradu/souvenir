package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	gossh "golang.org/x/crypto/ssh"
)

type User struct {
	Id      string
	Name    string
	IsAdmin bool
}

type Key struct {
	Fingerprint string
	User        string
	Key         string
	Comment     string
	CreatedAt   time.Time
}

type contextKey struct{}

// WithUser marks everything done under ctx as done for u. Stores refuse to
// read or write owned rows without it.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

func FromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok && u.Id != ""
}

// Id is the scoping helper: every query on owned rows filters by it.
func Id(ctx context.Context) (string, error) {
	u, ok := FromContext(ctx)
	if !ok {
		return "", errors.New("no user for this request")
	}
	return u.Id, nil
}

var ErrNotFound = errors.New("not found")

type Store struct {
	logger slog.Logger
	pool   *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{logger: *slog.Default().With("Component", "Users"), pool: pool}
}

func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, is_admin FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanUser)
}

func (s *Store) ByName(ctx context.Context, name string) (User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, is_admin FROM users WHERE name = $1`, name)
	return one(rows, err, fmt.Sprintf("user %q", name))
}

// Owner is the first admin: the user of the local TUI and of the keys in the
// authorized_keys file.
func (s *Store) Owner(ctx context.Context) (User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, is_admin FROM users WHERE is_admin ORDER BY created_at LIMIT 1`)
	return one(rows, err, "admin user")
}

func (s *Store) Add(ctx context.Context, name string, admin bool) (User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return User{}, errors.New("a user needs a name")
	}
	rows, err := s.pool.Query(ctx, `INSERT INTO users (name, is_admin) VALUES ($1, $2) RETURNING id, name, is_admin`, name, admin)
	u, err := one(rows, err, "user")
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return u, fmt.Errorf("user %q already exists", name)
	}
	return u, err
}

// Remove deletes a user with their conversations, memories and keys. The
// last admin stays, since the owner is the first admin.
func (s *Store) Remove(ctx context.Context, name string) error {
	u, err := s.ByName(ctx, name)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`
		DELETE FROM users
		 WHERE id = $1
		   AND (NOT is_admin OR (SELECT count(*) FROM users WHERE is_admin) > 1)
		`, u.Id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s is the last admin and cannot be removed", name)
	}
	return nil
}

// ByKey finds whose key this is.
func (s *Store) ByKey(ctx context.Context, key gossh.PublicKey) (User, error) {
	rows, err := s.pool.Query(ctx,
		`
		SELECT u.id, u.name, u.is_admin
		  FROM ssh_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.fingerprint = $1
		`, gossh.FingerprintSHA256(key))
	return one(rows, err, "key")
}

// AddKey takes a line in authorized_keys format.
func (s *Store) AddKey(ctx context.Context, userName, line string) (Key, error) {
	pub, comment, _, _, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return Key{}, fmt.Errorf("not a public key: %w", err)
	}
	k := Key{
		Fingerprint: gossh.FingerprintSHA256(pub),
		User:        userName,
		Key:         strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pub))),
		Comment:     comment,
	}
	tag, err := s.pool.Exec(ctx,
		`
		INSERT INTO ssh_keys (fingerprint, user_id, public_key, comment)
		     SELECT $1, id, $2, $3 FROM users WHERE name = $4
		`, k.Fingerprint, k.Key, k.Comment, userName)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return k, fmt.Errorf("key %s is already registered", k.Fingerprint)
	}
	if err != nil {
		return k, err
	}
	if tag.RowsAffected() == 0 {
		return k, fmt.Errorf("user %q: %w", userName, ErrNotFound)
	}
	return k, nil
}

func (s *Store) RemoveKey(ctx context.Context, fingerprint string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ssh_keys WHERE fingerprint = $1`, fingerprint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("key %s: %w", fingerprint, ErrNotFound)
	}
	return nil
}

func (s *Store) Keys(ctx context.Context) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		`
		  SELECT k.fingerprint, u.name, k.public_key, k.comment, k.created_at
		    FROM ssh_keys k
		    JOIN users u ON u.id = k.user_id
		ORDER BY u.name, k.created_at
		`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Key, error) {
		var k Key
		return k, row.Scan(&k.Fingerprint, &k.User, &k.Key, &k.Comment, &k.CreatedAt)
	})
}

func scanUser(row pgx.CollectableRow) (User, error) {
	var u User
	return u, row.Scan(&u.Id, &u.Name, &u.IsAdmin)
}

func one(rows pgx.Rows, err error, what string) (User, error) {
	if err != nil {
		return User{}, err
	}
	u, err := pgx.CollectOneRow(rows, scanUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return u, err
}

var ErrAlreadyLinked = errors.New("already linked to another login from this provider")

func (s *Store) ByIdentity(ctx context.Context, issuer, subject string) (User, error) {
	rows, err := s.pool.Query(ctx,
		`
		SELECT u.id, u.name, u.is_admin
		  FROM oidc_identities i
		  JOIN users u ON u.id = i.user_id
		 WHERE i.issuer = $1 AND i.subject = $2
		`, issuer, subject)
	return one(rows, err, "login")
}

func (s *Store) LinkIdentity(ctx context.Context, userId, issuer, subject string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oidc_identities (issuer, subject, user_id) VALUES ($1, $2, $3)`,
		issuer, subject, userId)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return ErrAlreadyLinked
	}
	return err
}

func (s *Store) Unlink(ctx context.Context, name string) (int64, error) {
	u, err := s.ByName(ctx, name)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM oidc_identities WHERE user_id = $1`, u.Id)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.Id); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}
