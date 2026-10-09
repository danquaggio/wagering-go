package bootstrap

import (
	"context"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/auth"
	"example.com/wagering/internal/config"
	"example.com/wagering/internal/httpapi"
	"example.com/wagering/internal/messaging"
	"example.com/wagering/internal/postgres"
	"example.com/wagering/internal/telemetry"
	"example.com/wagering/internal/workers"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"
)

func logger() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) }
func pool(lc fx.Lifecycle, c config.Config) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(c.DB)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = 20
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		if err := p.Ping(ctx); err != nil {
			return errors.New("PostgreSQL unavailable")
		}
		var v int
		if err := p.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&v); err != nil || v != 1 {
			return errors.New("apply migrations before starting")
		}
		return nil
	}, OnStop: func(context.Context) error { p.Close(); return nil }})
	return p, nil
}
func usecase(p *postgres.Store, c config.Config) (*application.Service, error) {
	return application.New(p, c.MaxAttempts, c.ReferenceTTL)
}
func readiness(b *messaging.Broker) httpapi.Readiness { return b }
func register(lc fx.Lifecycle, c config.Config, b *messaging.Broker, w *workers.Workers, a *httpapi.API, l *slog.Logger, shutdown fx.Shutdowner) {
	server := &http.Server{Addr: c.Address, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	var serverDone chan struct{}
	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		if err := b.Init(ctx); err != nil {
			return errors.New("SQS queues unavailable")
		}
		// Probe the external IdP JWKS endpoint; JWT validation itself stays in auth.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.JWKS, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return errors.New("IdP unavailable")
		}
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			return errors.New("IdP JWKS not ready")
		}
		listener, err := net.Listen("tcp", c.Address)
		if err != nil {
			return err
		}
		serverDone = make(chan struct{})
		w.Start()
		go func() {
			defer close(serverDone)
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				l.Error("HTTP server stopped unexpectedly")
				_ = shutdown.Shutdown(fx.ExitCode(1))
			}
		}()
		l.Info("application started", "address", listener.Addr().String())
		return nil
	}, OnStop: func(ctx context.Context) error {
		w.StopFetching()
		err := server.Shutdown(ctx)
		if err != nil {
			_ = server.Close()
		}
		workerErr := w.Stop(ctx)
		if serverDone != nil {
			select {
			case <-serverDone:
			case <-ctx.Done():
			}
		}
		return errors.Join(err, workerErr)
	}})
}
func Options() fx.Option {
	return fx.Options(
		fx.StartTimeout(30*time.Second), fx.StopTimeout(30*time.Second),
		fx.WithLogger(func(l *slog.Logger) fxevent.Logger { return &fxevent.SlogLogger{Logger: l} }),
		fx.Module("configuration", fx.Provide(config.Load, logger)),
		fx.Module("infrastructure", fx.Provide(pool, postgres.New, messaging.New, auth.New, telemetry.New, readiness)),
		fx.Module("application", fx.Provide(usecase)),
		fx.Module("transports", fx.Provide(httpapi.New, workers.New), fx.Invoke(register)),
	)
}
func New() *fx.App { return fx.New(Options()) }
