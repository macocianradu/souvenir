package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const loginTimeout = 10 * time.Minute

var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func Token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) Create(ctx context.Context, userId string, ttl time.Duration) (string, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return "", err
	}
	token := Token()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hash(token), userId, time.Now().Add(ttl))
	return token, err
}

func (s *Store) User(ctx context.Context, token string) (users.User, error) {
	var u users.User
	err := s.pool.QueryRow(ctx,
		`
		SELECT u.id, u.name, u.is_admin
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1 AND s.expires_at > now()
		`, hash(token)).Scan(&u.Id, &u.Name, &u.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) Delete(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash(token))
	return err
}

func (s *Store) BeginLogin(ctx context.Context, state, verifier, nonce string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM oidc_logins WHERE created_at < $1`, time.Now().Add(-loginTimeout)); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO oidc_logins (state, verifier, nonce) VALUES ($1, $2, $3)`, state, verifier, nonce)
	return err
}

func (s *Store) TakeLogin(ctx context.Context, state string) (verifier, nonce string, err error) {
	var fresh bool
	err = s.pool.QueryRow(ctx,
		`
		DELETE FROM oidc_logins
		 WHERE state = $1
		RETURNING verifier, nonce, created_at > $2
		`, state, time.Now().Add(-loginTimeout)).Scan(&verifier, &nonce, &fresh)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !fresh) {
		return "", "", ErrNotFound
	}
	return verifier, nonce, err
}
