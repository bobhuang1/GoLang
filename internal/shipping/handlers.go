package shipping

import (
	"context"
	"net/http"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Handler exposes the shipping endpoints.
type Handler struct {
	svc    *Service
	pool   *pgxpool.Pool
	tokens *auth.TokenManager
}

// NewHandler builds the shipping handlers.
func NewHandler(svc *Service, pool *pgxpool.Pool, tokens *auth.TokenManager) *Handler {
	return &Handler{svc: svc, pool: pool, tokens: tokens}
}

// CustomerRoutes are the customer-facing shipping/tracking endpoints.
func (h *Handler) CustomerRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens))
	r.Get("/", h.own)
	r.Get("/track", h.track)
	r.Get("/orders/{orderID}", h.byOrder)
}

// AdminRoutes are the admin fulfilment endpoints.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens), httpx.RequireAdmin)
	r.Post("/", h.ship)
	r.Put("/{id}", h.update)
}

func (h *Handler) own(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	items, err := h.svc.Own(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"shipments": items})
}

func (h *Handler) track(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		httpx.WriteError(w, httpx.BadRequest("code query parameter is required"))
		return
	}
	sh, err := h.svc.Track(r.Context(), code)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, sh)
}

func (h *Handler) byOrder(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	orderID, err := httpx.ParsePathID(r, "orderID")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if !h.owns(r.Context(), user.CustomerID, orderID) {
		httpx.WriteError(w, httpx.Forbidden("not allowed to view this shipment"))
		return
	}
	sh, err := h.svc.ByOrder(r.Context(), orderID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, sh)
}

func (h *Handler) ship(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID      int64  `json:"order_id"`
		Courier      string `json:"courier"`
		TrackingCode string `json:"tracking_code"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if in.OrderID == 0 {
		httpx.WriteError(w, httpx.BadRequest("order_id is required"))
		return
	}
	sh, err := h.svc.Ship(r.Context(), in.OrderID, in.Courier, in.TrackingCode)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, sh)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		Status       string `json:"status"`
		TrackingCode string `json:"tracking_code"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	sh, err := h.svc.Update(r.Context(), id, in.TrackingCode, in.Status)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, sh)
}

func (h *Handler) owns(ctx context.Context, customerID, orderID int64) bool {
	var owner int64
	err := h.pool.QueryRow(ctx,
		`SELECT customer_id FROM orders WHERE id = $1`, orderID).Scan(&owner)
	if err != nil {
		return false
	}
	return owner == customerID
}
