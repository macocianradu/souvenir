package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	"charm.land/wish/v2/activeterm"
	"charm.land/wish/v2/bubbletea"
	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	gossh "golang.org/x/crypto/ssh"
)

type sessionUserKey struct{}

func serve(ctx context.Context, cfg config.SshConfig, accounts *users.Store, newModel func(context.Context) tea.Model) error {
	logger := slog.Default().With("Component", "SSH")
	if cfg.AuthorizedKeys != "" {
		if _, err := os.Stat(cfg.AuthorizedKeys); err != nil {
			return fmt.Errorf("Ssh.AuthorizedKeys must point to an authorized_keys file of the owner's keys, or be empty to allow only registered keys: %w", err)
		}
	}
	userFor := func(ctx context.Context, key ssh.PublicKey) (users.User, error) {
		u, err := accounts.ByKey(ctx, key)
		if !errors.Is(err, users.ErrNotFound) || cfg.AuthorizedKeys == "" {
			return u, err
		}
		listed, ferr := inAuthorizedKeys(cfg.AuthorizedKeys, key)
		if ferr != nil {
			return u, ferr
		}
		if !listed {
			return u, err
		}
		return accounts.Owner(ctx)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.HostKeyPath), 0o700); err != nil {
		return err
	}

	server, err := wish.NewServer(
		wish.WithAddress(cfg.Listen),
		wish.WithHostKeyPath(cfg.HostKeyPath),
		wish.WithPublicKeyAuth(func(sctx ssh.Context, key ssh.PublicKey) bool {
			_, err := userFor(sctx, key)
			if err != nil && !errors.Is(err, users.ErrNotFound) {
				logger.Error("Could not check key", "key", gossh.FingerprintSHA256(key), "error", err)
			}
			return err == nil
		}),
		wish.WithMiddleware(
			bubbletea.Middleware(func(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
				u := sess.Context().Value(sessionUserKey{}).(users.User)
				return newModel(users.WithUser(sess.Context(), u)), nil
			}),
			activeterm.Middleware(),
			func(next ssh.Handler) ssh.Handler {
				return func(sess ssh.Session) {
					fingerprint := gossh.FingerprintSHA256(sess.PublicKey())
					// Resolved again from the key the session was opened
					// with, since auth may have been asked about others.
					u, err := userFor(sess.Context(), sess.PublicKey())
					if err != nil {
						logger.Error("No user for the session's key", "key", fingerprint, "error", err)
						fmt.Fprintln(sess.Stderr(), "This key is not registered.")
						sess.Exit(1)
						return
					}
					sess.Context().SetValue(sessionUserKey{}, u)
					logger.Info("Session started", "user", u.Name, "key", fingerprint, "remote", sess.RemoteAddr().String())
					start := time.Now()
					next(sess)
					logger.Info("Session ended", "user", u.Name, "key", fingerprint, "duration", time.Since(start).Round(time.Second))
				}
			},
		),
	)
	if err != nil {
		return err
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("Listening", "address", cfg.Listen)
		errs <- server.ListenAndServe()
	}()
	select {
	case err := <-errs:
		if errors.Is(err, ssh.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	logger.Info("Shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		logger.Warn("Closing sessions still open", "error", err)
		if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

func inAuthorizedKeys(path string, key ssh.PublicKey) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for len(data) > 0 {
		var listed ssh.PublicKey
		listed, _, _, data, err = gossh.ParseAuthorizedKey(data)
		if err != nil {
			// Only blank lines and comments are left.
			return false, nil
		}
		if ssh.KeysEqual(listed, key) {
			return true, nil
		}
	}
	return false, nil
}
