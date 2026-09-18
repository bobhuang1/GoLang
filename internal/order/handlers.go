package order

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// Handler exposes the order endpoints.
type Handler struct {
	svc    *Service
	pool   *pgxpool.Pool
	tokens *auth.TokenManager
}

// NewHandler builds the order handlers.
func NewHandler(svc *Service, pool *pgxpool.Pool, tokens *auth.TokenManager) *Handler {
	return &Handler{svc: svc, pool: pool, tokens: tokens}
}

// CustomerRoutes are the customer-facing order status endpoints.
func (h *Handler) CustomerRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens))
	r.Get("/", h.own)
	r.Get("/{id}", h.detail)
}

// AdminRoutes are the admin order-management endpoints.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens), httpx.RequireAdmin)
	r.Get("/", h.list)
	r.Post("/{id}/process", h.process)
}

func (h *Handler) own(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	views, err := h.svc.Own(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"orders": views})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	status := Status(r.URL.Query().Get("status"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	views, err := h.svc.List(r.Context(), status, page, limit)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"orders": views})
}

// paymentSummary and shipmentSummary are the enrichments attached to a detail
// response; either can be nil when the order has no payment / no shipment yet.
type paymentSummary struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key"`
}

type shipmentSummary struct {
	ID           string `json:"id"`
	TrackingCode string `json:"tracking_code"`
	Courier      string `json:"courier"`
	Status       string `json:"status"`
}

// detail builds a rich order view while fetching the order lines, the payment
// and the shipment concurrently with errgroup (the sample's "parallel reads"
// showcase).
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParsePathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	o, err := h.svc.ByID(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	user := httpx.UserFrom(r.Context())
	if o.CustomerID != user.CustomerID && user.Role != auth.RoleAdmin {
		httpx.WriteError(w, httpx.Forbidden("not allowed to view this order"))
		return
	}

	var g errgroup.Group
	view := &View{Order: *o}

	g.Go(func() error {
		items, err := h.svc.Items(r.Context(), id)
		if err != nil {
			return err
		}
		view.Items = items
		return nil
	})

	var pay *paymentSummary
	g.Go(func() error {
		var p paymentSummary
		err := h.pool.QueryRow(r.Context(), `
			SELECT id::text, status::text, amount_cents, idempotency_key
			FROM charges WHERE order_id = $1`, id,
		).Scan(&p.ID, &p.Status, &p.AmountCents, &p.IdempotencyKey)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				pay = nil
				return nil
			}
			return err
		}
		pay = &p
		return nil
	})

	var ship *shipmentSummary
	g.Go(func() error {
		var s shipmentSummary
		err := h.pool.QueryRow(r.Context(), `
			SELECT id::text, tracking_code, courier, status::text
			FROM shipments WHERE order_id = $1`, id,
		).Scan(&s.ID, &s.TrackingCode, &s.Courier, &s.Status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				ship = nil
				return nil
			}
			return err
		}
		ship = &s
		return nil
	})

	if err := g.Wait(); err != nil {
		httpx.WriteError(w, httpx.Wrap(err))
		return
	}

	httpx.WriteOK(w, map[string]any{
		"order":    view,
		"payment":  pay,
		"shipment": ship,
	})
}

// process implements the "admin order processing" endpoint: it applies one of
// the allowed actions to an order through the concurrency-safe state machine.
func (h *Handler) process(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParsePathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	view, err := h.svc.Process(r.Context(), id, Action(in.Action))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, view)
}
