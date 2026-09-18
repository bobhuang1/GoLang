package product

import (
	"net/http"
	"strconv"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Handler exposes the product endpoints.
type Handler struct {
	svc *Service
}

// NewHandler builds the product handlers.
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// PublicRoutes are the customer-facing product endpoints.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
}

// AdminRoutes are the admin product-management endpoints.
func (h *Handler) AdminRoutes(r chi.Router, tokens *auth.TokenManager) {
	r.Use(httpx.RequireAuth(tokens), httpx.RequireAdmin)
	r.Post("/", h.create)
	r.Put("/{id}", h.update)
	r.Delete("/{id}", h.delete)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	q := r.URL.Query().Get("q")
	items, err := h.svc.List(r.Context(), page, limit, q)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]any{"products": items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParsePathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, p)
}

type createInput struct {
	SKU         string `json:"sku"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceCents  int64  `json:"price_cents"`
	Currency    string `json:"currency"`
	Stock       int    `json:"stock"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in createInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	p, err := h.svc.Create(r.Context(), in.SKU, in.Name, in.Description, in.Currency, in.PriceCents, in.Stock)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, p)
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParsePathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var in UpdateInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	p, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, p)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.ParsePathID(r, "id")
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := h.svc.Deactivate(r.Context(), id); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteNoContent(w)
}
