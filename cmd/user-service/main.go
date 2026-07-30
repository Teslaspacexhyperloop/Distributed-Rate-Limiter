// Command user-service is a mock backend used to exercise the gateway's
// reverse proxy in Phase 1.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"distributed-rate-limiter/internal/config"
	custommw "distributed-rate-limiter/internal/middleware"
)

type user struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

var users = []user{
	{ID: "1", Name: "Ada Lovelace", Email: "ada@example.com"},
	{ID: "2", Name: "Grace Hopper", Email: "grace@example.com"},
	{ID: "3", Name: "Alan Turing", Email: "alan@example.com"},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.LoadBackend("user-service", "8081")

	r := chi.NewRouter()
	r.Use(custommw.RequestID)
	r.Use(custommw.Logging(logger))
	r.Use(chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Get("/api/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, users)
	})

	r.Get("/api/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		for _, u := range users {
			if u.ID == id {
				writeJSON(w, http.StatusOK, u)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
	})

	logger.Info("user-service listening", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, r); err != nil {
		logger.Error("user-service server error", "error", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
