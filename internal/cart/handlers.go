package cart

import (
	"net/http"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Handler exposes the cart endpoints.
type Handler struct {
	svc    *Service
	tokens *auth.TokenManager
}

// NewHandler builds the cart handlers.
func NewHandler(svc *Service, tokens *auth.TokenManager) *Handler {
	return &Handler{svc: svc, tokens: tokens}
}

// Routes are the customer-facing cart endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Use(httpx.RequireAuth(h.tokens))
	r.Get("/", h.view)
	r.Delete("/", h.clear)
	r.Post("/items", h.add)
	r.Put("/items/{productID}", h.update)
	r.Delete("/items/{productID}", h.remove)
	r.Post("/checkout", h.checkout)
}

func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	v, err := h.svc.View(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, v)
}

func (h *Handler) clear(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	if err := h.svc.Clear(r.Context(), user.CustomerID); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteNoContent(w)
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	var in struct {
		ProductID int64 `json:"product_id"`
		Qty       int   `json:"qty"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if err := h.svc.Add(r.Context(), user.CustomerID, in.ProductID, in.Qty); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, map[string]any{"product_id": in.ProductID, "qty": in.Qty})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	productID, err := httpx.ParsePathID(r, "productID")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in struct {
		Qty int `json:"qty"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if in.Qty <= 0 {
		httpx.WriteError(w, h.svc.Remove(r.Context(), user.CustomerID, productID))
		return
	}
	httpx.WriteError(w, h.svc.Update(r.Context(), user.CustomerID, productID, in.Qty))
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	productID, err := httpx.ParsePathID(r, "productID")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteError(w, h.svc.Remove(r.Context(), user.CustomerID, productID))
}

func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	key := r.Header.Get("Idempotency-Key")
	v, err := h.svc.Checkout(r.Context(), user.CustomerID, key)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, v)
}
