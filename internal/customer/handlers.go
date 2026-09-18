package customer

import (
	"net/http"
	"strconv"

	"github.com/bobhuang1/GoLang/internal/auth"
	"github.com/bobhuang1/GoLang/internal/httpx"
	"github.com/go-chi/chi/v5"
)

// Handler exposes the customer + auth HTTP endpoints.
type Handler struct {
	svc    *Service
	tokens *auth.TokenManager
}

// NewHandler builds the customer route handler.
func NewHandler(svc *Service, tokens *auth.TokenManager) *Handler {
	return &Handler{svc: svc, tokens: tokens}
}

// Routes mounts both the /auth and /customers route groups.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/auth", func(r chi.Router) {
		r.Post("/login", h.login)
		r.Post("/login/2fa", h.login2FA)
		r.Post("/logout", h.logout)

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireAuth(h.tokens))
			r.Post("/2fa/enroll", h.enroll2FA)
			r.Post("/2fa/activate", h.activate2FA)
			r.Post("/2fa/disable", h.disable2FA)
		})
	})

	r.Route("/customers", func(r chi.Router) {
		r.Post("/", h.register)
		r.Post("/forgot-password", h.forgotPassword)
		r.Post("/reset-password", h.resetPassword)

		r.Group(func(r chi.Router) {
			r.Use(httpx.RequireAuth(h.tokens))
			r.Get("/me", h.me)
			r.Get("/{id}", h.requireOwnOrAdmin(h.get))
			r.Put("/{id}", h.requireOwnOrAdmin(h.update))
			r.Delete("/{id}", h.requireOwnOrAdmin(h.delete))
		})
	})
}

// requireOwnOrAdmin wraps a handler that receives the path id, allowing the
// owner themselves or any admin to proceed.
func (h *Handler) requireOwnOrAdmin(next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := httpx.ParsePathID(r, "id")
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		user := httpx.UserFrom(r.Context())
		if user == nil {
			httpx.WriteError(w, httpx.Unauthorized("authentication required"))
			return
		}
		if user.CustomerID != id && user.Role != auth.RoleAdmin {
			httpx.WriteError(w, httpx.Forbidden("not allowed to access this account"))
			return
		}
		next(w, r, id)
	}
}

// ----- register / profile ---------------------------------------------------

type registerInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in registerInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	c, err := h.svc.Register(r.Context(), in.Email, in.Password, in.FullName)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteCreated(w, c)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	c, err := h.svc.Get(r.Context(), user.CustomerID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, c)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id int64) {
	c, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, c)
}

type updateProfileInput struct {
	FullName string `json:"full_name"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, id int64) {
	var in updateProfileInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	c, err := h.svc.UpdateProfile(r.Context(), id, in.FullName)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, c)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.Delete(r.Context(), id); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteNoContent(w)
}

// ----- forgot / reset password ----------------------------------------------

type forgotPasswordInput struct {
	Email string `json:"email"`
}

func (h *Handler) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var in forgotPasswordInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	code, err := h.svc.ForgotPassword(r.Context(), in.Email)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	// Demo only: the code would be emailed. Returning it makes the sample
	// runnable end-to-end without an SMTP server.
	httpx.WriteOK(w, map[string]any{
		"message": "reset code issued",
		"code":    code,
		"note":    "demo: the code would normally be emailed to " + in.Email,
	})
}

type resetPasswordInput struct {
	Email       string `json:"email"`
	Code        string `json:"code"`
	NewPassword string `json:"new_password"`
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in resetPasswordInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	if err := h.svc.ResetPassword(r.Context(), in.Email, in.Code, in.NewPassword); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]string{"message": "password updated"})
}

// ----- login / 2FA ----------------------------------------------------------

type loginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken       string    `json:"access_token,omitempty"`
	TokenType         string    `json:"token_type,omitempty"`
	TwoFactorRequired bool      `json:"two_factor_required"`
	ChallengeToken    string    `json:"challenge_token,omitempty"`
	Customer          *Customer `json:"customer,omitempty"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in loginInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	row, err := h.svc.GetWithCredentials(r.Context(), in.Email)
	if err != nil {
		// Uniform message to avoid leaking account existence.
		httpx.WriteError(w, httpx.Unauthorized("invalid email or password"))
		return
	}
	if !auth.CheckPassword(row.PasswordHash, in.Password) {
		httpx.WriteError(w, httpx.Unauthorized("invalid email or password"))
		return
	}

	c := h.svc.toCustomer(*row)
	if !c.TOTPEnabled {
		token, err := h.tokens.Sign(c.ID, c.Email, roleOf(c.IsAdmin))
		if err != nil {
			httpx.WriteError(w, httpx.Wrap(err))
			return
		}
		httpx.WriteOK(w, loginResponse{
			AccessToken: token, TokenType: "Bearer",
			TwoFactorRequired: false, Customer: c,
		})
		return
	}

	// Password verified but 2FA is enabled: hand out a short-lived challenge.
	challenge, err := h.tokens.SignChallenge(c.ID, c.Email, roleOf(c.IsAdmin))
	if err != nil {
		httpx.WriteError(w, httpx.Wrap(err))
		return
	}
	httpx.WriteOK(w, loginResponse{
		TwoFactorRequired: true, ChallengeToken: challenge,
	})
}

type login2FAInput struct {
	ChallengeToken string `json:"challenge_token"`
	Code           string `json:"code"`
}

func (h *Handler) login2FA(w http.ResponseWriter, r *http.Request) {
	var in login2FAInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	claims, err := h.tokens.Parse(in.ChallengeToken)
	if err != nil || !auth.ChallengeKind(claims) {
		httpx.WriteError(w, httpx.Unauthorized("invalid challenge token"))
		return
	}
	subject, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil {
		httpx.WriteError(w, httpx.Unauthorized("invalid challenge token"))
		return
	}
	if err := h.svc.Verify2FA(r.Context(), subject, claims.Email, in.Code, false); err != nil {
		httpx.WriteError(w, err)
		return
	}
	token, err := h.tokens.Sign(subject, claims.Email, claims.Role)
	if err != nil {
		httpx.WriteError(w, httpx.Wrap(err))
		return
	}
	httpx.WriteOK(w, loginResponse{AccessToken: token, TokenType: "Bearer", TwoFactorRequired: false})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	// Stateless JWTs: nothing to invalidate server-side.
	httpx.WriteOK(w, map[string]string{"message": "logged out"})
}

type enroll2FAResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauth_url"`
	ManualKey  string `json:"manual_key"`
}

func (h *Handler) enroll2FA(w http.ResponseWriter, r *http.Request) {
	user := httpx.UserFrom(r.Context())
	secret, url, err := h.svc.Enroll2FA(r.Context(), user.CustomerID, user.Email)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, enroll2FAResponse{Secret: secret, OtpauthURL: url, ManualKey: secret})
}

type codeInput struct {
	Code string `json:"code"`
}

func (h *Handler) activate2FA(w http.ResponseWriter, r *http.Request) {
	var in codeInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	user := httpx.UserFrom(r.Context())
	if err := h.svc.Verify2FA(r.Context(), user.CustomerID, user.Email, in.Code, true); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]string{"message": "2FA enabled"})
}

func (h *Handler) disable2FA(w http.ResponseWriter, r *http.Request) {
	var in codeInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return
	}
	user := httpx.UserFrom(r.Context())
	if err := h.svc.Disable2FA(r.Context(), user.CustomerID, in.Code); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteOK(w, map[string]string{"message": "2FA disabled"})
}

func roleOf(isAdmin bool) string {
	if isAdmin {
		return auth.RoleAdmin
	}
	return auth.RoleCustomer
}
