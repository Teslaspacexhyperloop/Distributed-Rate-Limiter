// Command order-service is a mock backend used to exercise the gateway's
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

type order struct {
	ID        string `json:"id"`
	UserID    string `json:"userId"`
	ProductID string `json:"productId"`
	Quantity  int    `json:"quantity"`
	Status    string `json:"status"`
}

var orders = []order{
	{ID: "1", UserID: "1", ProductID: "2", Quantity: 1, Status: "shipped"},
	{ID: "2", UserID: "2", ProductID: "1", Quantity: 2, Status: "processing"},
	{ID: "3", UserID: "3", ProductID: "3", Quantity: 3, Status: "delivered"},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.LoadBackend("order-service", "8083")

	r := chi.NewRouter()
	r.Use(custommw.RequestID)
	r.Use(custommw.Logging(logger))
	r.Use(chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Get("/api/orders", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, orders)
	})

	r.Get("/api/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		for _, o := range orders {
			if o.ID == id {
				writeJSON(w, http.StatusOK, o)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "order not found"})
	})

	logger.Info("order-service listening", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, r); err != nil {
		logger.Error("order-service server error", "error", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
