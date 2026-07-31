package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// Handler serves the /auth/register and /auth/login endpoints.
// Users are stored in Redis so all gateway instances share the same user store —
// a user registered through Gateway 1 can log in through Gateway 2.
type Handler struct {
	rdb       *redis.Client
	jwtSecret string
	tokenTTL  time.Duration
}

// NewHandler creates an auth Handler.
func NewHandler(rdb *redis.Client, jwtSecret string, tokenTTL time.Duration) *Handler {
	return &Handler{rdb: rdb, jwtSecret: jwtSecret, tokenTTL: tokenTTL}
}

type registerRequest struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	Plan      string `json:"plan"`      // "free" | "pro" | "enterprise"
	Algorithm string `json:"algorithm"` // optional preferred algorithm
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenResponse struct {
	Token string `json:"token"`
	Plan  string `json:"plan"`
	Sub   string `json:"sub"`
}

// Register creates a new user and returns a JWT.
// POST /auth/register
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password required"})
		return
	}
	if req.Plan == "" {
		req.Plan = "free"
	}

	userKey := "user:" + req.Username
	exists, err := h.rdb.Exists(r.Context(), userKey).Result()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}
	if exists > 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "username already taken"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not hash password"})
		return
	}

	userID := newID()
	if err := h.rdb.HSet(r.Context(), userKey,
		"id", userID,
		"password_hash", string(hash),
		"plan", req.Plan,
		"algorithm", req.Algorithm,
	).Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save user"})
		return
	}

	token, err := h.issueToken(r.Context(), userID, req.Plan, req.Algorithm)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not sign token"})
		return
	}

	writeJSON(w, http.StatusCreated, tokenResponse{Token: token, Plan: req.Plan, Sub: userID})
}

// Login validates credentials and returns a JWT.
// POST /auth/login
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	userKey := "user:" + req.Username
	fields, err := h.rdb.HGetAll(r.Context(), userKey).Result()
	if err != nil || len(fields) == 0 {
		// Return the same error for unknown user and wrong password to prevent enumeration.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(fields["password_hash"]), []byte(req.Password)); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}

	token, err := h.issueToken(r.Context(), fields["id"], fields["plan"], fields["algorithm"])
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not sign token"})
		return
	}

	writeJSON(w, http.StatusOK, tokenResponse{Token: token, Plan: fields["plan"], Sub: fields["id"]})
}

func (h *Handler) issueToken(_ context.Context, userID, plan, algorithm string) (string, error) {
	return Sign(userID, plan, algorithm, h.jwtSecret, h.tokenTTL)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b)
}
