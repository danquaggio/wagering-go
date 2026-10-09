package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/auth"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/postgres"
	"example.com/wagering/internal/telemetry"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type Readiness interface{ Ready(context.Context) error }
type API struct {
	service *application.Service
	store   *postgres.Store
	auth    *auth.Auth
	queue   Readiness
	metrics *telemetry.Metrics
	log     *slog.Logger
}

func New(s *application.Service, p *postgres.Store, a *auth.Auth, q Readiness, m *telemetry.Metrics, l *slog.Logger) *API {
	return &API{s, p, a, q, m, l}
}
func write(w http.ResponseWriter, status int, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		http.Error(w, "serialization error", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
func fail(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"failureCode": code})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return domain.Rule("INVALID_INPUT")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return domain.Rule("INVALID_INPUT")
	}
	return nil
}
func (a *API) error(w http.ResponseWriter, r *http.Request, err error) {
	var rule *domain.RuleError
	switch {
	case errors.As(err, &rule):
		fail(w, 400, rule.Code)
	case errors.Is(err, application.ErrConflict):
		fail(w, 409, "IDEMPOTENCY_CONFLICT")
	case errors.Is(err, application.ErrNotFound):
		fail(w, 404, "NOT_FOUND")
	default:
		var pg *pgconn.PgError
		if errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001" || pg.Code == "55P03") {
			a.metrics.Conflicts.Add(1)
		}
		// Do not print PG error Detail: it can contain rows, amounts or credentials.
		a.log.Error("request infrastructure failure", "correlationId", r.Header.Get("X-Correlation-ID"))
		w.Header().Set("Retry-After", "1")
		fail(w, 503, "TEMPORARILY_UNAVAILABLE")
	}
}
func (a *API) protected(internal bool, next func(http.ResponseWriter, *http.Request, auth.Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		correlation := r.Header.Get("X-Correlation-ID")
		if !domain.ValidToken(correlation) {
			correlation = domain.ID()
		}
		r.Header.Set("X-Correlation-ID", correlation)
		w.Header().Set("X-Correlation-ID", correlation)
		identity, err := a.auth.Verify(ctx, r.Header.Get("Authorization"))
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, 401, "UNAUTHENTICATED")
			return
		}
		if internal != identity.Internal {
			fail(w, 403, "FORBIDDEN")
			return
		}
		next(w, r, identity)
	}
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if a.store.Pool.Ping(ctx) != nil || a.queue.Ready(ctx) != nil {
			fail(w, 503, "NOT_READY")
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", a.protected(true, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) { a.metrics.ServeHTTP(w, r) }))
	mux.HandleFunc("POST /wallets", a.protected(true, a.open))
	mux.HandleFunc("GET /wallets/{id}", a.protected(true, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		id := r.PathValue("id")
		if !domain.ValidID(id) {
			fail(w, 400, "INVALID_WALLET_ID")
			return
		}
		v, err := a.store.Wallet(r.Context(), id)
		if err != nil {
			a.error(w, r, err)
			return
		}
		write(w, 200, v.View())
	}))
	mux.HandleFunc("GET /wallets/{id}/ledger", a.protected(true, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		id := r.PathValue("id")
		if !domain.ValidID(id) {
			fail(w, 400, "INVALID_WALLET_ID")
			return
		}
		limit, err := postgres.Limit(r.URL.Query().Get("limit"))
		if err != nil {
			a.error(w, r, err)
			return
		}
		p, err := a.store.Ledger(r.Context(), id, r.URL.Query().Get("cursor"), limit)
		if err != nil {
			a.error(w, r, err)
			return
		}
		write(w, 200, p)
	}))
	mux.HandleFunc("POST /wallets/{id}/reconciliation", a.protected(true, func(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
		id := r.PathValue("id")
		if !domain.ValidID(id) {
			fail(w, 400, "INVALID_WALLET_ID")
			return
		}
		p, err := a.store.Reconcile(r.Context(), id)
		if err != nil {
			a.error(w, r, err)
			return
		}
		if !p.Consistent {
			a.metrics.Reconciliation.Add(1)
			a.log.Error("reconciliation divergence", "walletId", id, "correlationId", r.Header.Get("X-Correlation-ID"))
		}
		write(w, 200, p)
	}))
	mux.HandleFunc("POST /wagering/transactions", a.protected(false, a.execute))
	mux.HandleFunc("GET /wagering/transactions/{id}", a.protected(false, func(w http.ResponseWriter, r *http.Request, i auth.Identity) {
		if !domain.ValidID(r.PathValue("id")) {
			fail(w, 400, "INVALID_TRANSACTION_ID")
			return
		}
		t, err := a.store.Transaction(r.Context(), r.PathValue("id"), i.Provider)
		if err != nil {
			a.error(w, r, err)
			return
		}
		write(w, 200, t.View())
	}))
	mux.HandleFunc("GET /providers/{provider}/wagering/transactions/{external}", a.protected(false, func(w http.ResponseWriter, r *http.Request, i auth.Identity) {
		if r.PathValue("provider") != i.Provider {
			fail(w, 403, "FORBIDDEN")
			return
		}
		t, err := a.store.External(r.Context(), i.Provider, r.PathValue("external"))
		if err != nil {
			a.error(w, r, err)
			return
		}
		write(w, 200, t.View())
	}))
	return mux
}
func (a *API) open(w http.ResponseWriter, r *http.Request, _ auth.Identity) {
	var body struct {
		Player  string       `json:"playerId"`
		Initial domain.Money `json:"initialBalance"`
	}
	if err := decode(w, r, &body); err != nil {
		a.error(w, r, err)
		return
	}
	v, err := a.service.Open(r.Context(), body.Player, body.Initial)
	if errors.Is(err, application.ErrConflict) {
		fail(w, 409, "WALLET_ALREADY_EXISTS")
		return
	}
	if err != nil {
		a.error(w, r, err)
		return
	}
	write(w, 201, v)
}
func (a *API) execute(w http.ResponseWriter, r *http.Request, i auth.Identity) {
	start := time.Now()
	var body domain.Request
	if err := decode(w, r, &body); err != nil {
		a.error(w, r, err)
		return
	}
	if body.ProviderID != i.Provider {
		fail(w, 403, "FORBIDDEN")
		return
	}
	in := application.Input{Request: body, Key: r.Header.Get("Idempotency-Key"), CorrelationID: r.Header.Get("X-Correlation-ID")}
	result, err := a.service.Execute(r.Context(), in)
	if err != nil {
		a.error(w, r, err)
		return
	}
	a.metrics.Observe(string(result.Status), result.Replay, start)
	a.log.Info("transaction result", "correlationId", in.CorrelationID, "transactionId", result.TransactionID, "walletId", body.WalletID, "providerId", i.Provider, "status", result.Status, "replay", result.Replay)
	status := 200
	switch result.Status {
	case domain.Pending, domain.PendingReference:
		status = 202
	case domain.Rejected:
		status = 422
	case domain.Failed:
		status = 500
	}
	write(w, status, result)
}
