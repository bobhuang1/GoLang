package payment

import (
	"net/http"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Handler exposes the payment endpoints.
type Handler struct {
	svc    *Service
	tokens *auth.TokenManager
}

// NewHandler builds the payment handlers.
func NewHandler(svc *Service, tokens *auth.TokenManager) *Handler {
	return &Handler{svc: svc, tokens: tokens}
}

// CustomerRoutes are the caller-facing payment endpoints.
func (h *Handler) CustomerRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens))
	r.Get("/charges", h.myCharges)
	r.Post("/charges", h.charge)
	r.Get("/charges/{id}", h.getCharge)
	r.Post("/refunds", h.refund)
	r.Get("/refunds", h.myRefunds)
}

// AdminRoutes are the admin payment-management endpoints (refund processing).
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens), httpx.RequireAdmin)
	r.Post("/refunds", h.adminRefund)
}

func idemKey(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return "", httpx.BadRequest("Idempotency-Key header is required")
	}
	return k, nil
}

// POST /charges
func (h *Handler) charge(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		OrderID  int64  `json:"order_id"`
		Simulate string `json:"simulate"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if in.OrderID == 0 {
		httpx.WriteError(w, httpx.BadRequest("order_id is required"))
		return
	}
	user := httpx.UserFrom(r.Context())
	c, err := h.svc.Charge(r.Context(), in.OrderID, user.CustomerID, key, in.Simulate)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, c)
}

func (h *Handler) myCharges(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	items, err := h.svc.MyCharges(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"charges": items})
}

func (h *Handler) getCharge(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	user := httpx.UserFrom(r.Context())
	c, err := h.svc.ChargeByID(r.Context(), id, user.CustomerID, user.Role == auth.RoleAdmin)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, c)
}

// POST /refunds  (customer-initiated)
func (h *Handler) refund(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		OrderID     int64  `json:"order_id"`
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if in.OrderID == 0 {
		httpx.WriteError(w, httpx.BadRequest("order_id is required"))
		return
	}
	user := httpx.UserFrom(r.Context())
	re, err := h.svc.Refund(r.Context(), in.OrderID, user.CustomerID, in.AmountCents, key, in.Reason, false)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, re)
}

func (h *Handler) myRefunds(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	items, err := h.svc.MyRefunds(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"refunds": items})
}

// POST /admin/refunds  (admin-initiated)
func (h *Handler) adminRefund(w http.ResponseWriter, r *http.Request) {
	key, err := idemKey(r)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		OrderID     int64  `json:"order_id"`
		AmountCents int64  `json:"amount_cents"`
		Reason      string `json:"reason"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if in.OrderID == 0 {
		httpx.WriteError(w, httpx.BadRequest("order_id is required"))
		return
	}
	user := httpx.UserFrom(r.Context())
	re, err := h.svc.Refund(r.Context(), in.OrderID, user.CustomerID, in.AmountCents, key, in.Reason, true)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, re)
}
