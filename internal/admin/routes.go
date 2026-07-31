package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Mount registers all admin routes under the given Chi router.
// Admin routes are deliberately unauthenticated in Phase 3 — they should be
// behind an internal network or NGINX IP restriction in production. Phase 4
// adds NGINX; a separate admin JWT or mTLS would be a natural Phase 3 extension.
func Mount(r chi.Router, h *Handler) {
	r.Get("/admin/keys", h.ListKeys)
	r.Post("/admin/config/reload", h.Reload)
	r.Get("/admin/config/stats", h.Stats)

	// Wildcard routes capture keys that contain colons and forward slashes
	// (e.g. /admin/limits/rate-limit:user_123:/api/products).
	r.Get("/admin/limits/*", h.GetLimit)
	r.Put("/admin/limits/*", h.SetLimit)
	r.Delete("/admin/limits/*", h.DeleteLimit)

	// Convenience: list limits redirects to keys
	r.Get("/admin/limits", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/keys", http.StatusMovedPermanently)
	})
}
