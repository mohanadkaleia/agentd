package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"agentd/internal/api"
	"agentd/internal/app"
)

func startAPI(ctx context.Context, a *app.App, addr, tokenFile string, log *slog.Logger) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	token, err := loadOrCreateToken(tokenFile)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           (&api.Server{App: a, Token: token, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api server", "err", err)
		}
	}()
	log.Info("api listening", "addr", ln.Addr().String(), "token_file", tokenFile)
	return func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}, nil
}
