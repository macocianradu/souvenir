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
	gossh "golang.org/x/crypto/ssh"
)

func serve(ctx context.Context, cfg config.SshConfig, newModel func(context.Context) tea.Model) error {
	logger := slog.Default().With("Component", "SSH")
	if _, err := os.Stat(cfg.AuthorizedKeys); err != nil {
		return fmt.Errorf("Ssh.AuthorizedKeys must point to an authorized_keys file listing who may connect: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.HostKeyPath), 0o700); err != nil {
		return err
	}

	server, err := wish.NewServer(
		wish.WithAddress(cfg.Listen),
		wish.WithHostKeyPath(cfg.HostKeyPath),
		wish.WithAuthorizedKeys(cfg.AuthorizedKeys),
		wish.WithMiddleware(
			bubbletea.Middleware(func(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
				return newModel(sess.Context()), nil
			}),
			activeterm.Middleware(),
			func(next ssh.Handler) ssh.Handler {
				return func(sess ssh.Session) {
					fingerprint := gossh.FingerprintSHA256(sess.PublicKey())
					logger.Info("Session started", "user", sess.User(), "key", fingerprint, "remote", sess.RemoteAddr().String())
					start := time.Now()
					next(sess)
					logger.Info("Session ended", "user", sess.User(), "key", fingerprint, "duration", time.Since(start).Round(time.Second))
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
