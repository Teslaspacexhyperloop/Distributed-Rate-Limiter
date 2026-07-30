// Command product-service is a mock backend used to exercise the gateway's
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

type product struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Stock int     `json:"stock"`
}

var products = []product{
	{ID: "1", Name: "Mechanical Keyboard", Price: 89.99, Stock: 42},
	{ID: "2", Name: "27in 4K Monitor", Price: 349.00, Stock: 17},
	{ID: "3", Name: "USB-C Dock", Price: 59.50, Stock: 130},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.LoadBackend("product-service", "8082")

	r := chi.NewRouter()
	r.Use(custommw.RequestID)
	r.Use(custommw.Logging(logger))
	r.Use(chimw.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r.Get("/api/products", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, products)
	})

	r.Get("/api/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		for _, p := range products {
			if p.ID == id {
				writeJSON(w, http.StatusOK, p)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "product not found"})
	})

	logger.Info("product-service listening", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, r); err != nil {
		logger.Error("product-service server error", "error", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
