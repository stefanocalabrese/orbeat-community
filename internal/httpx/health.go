// Package httpx contains shared HTTP helpers for orbeat services.
package httpx

import (
	"encoding/json"
	"net/http"
)

// HealthHandler returns a handler that reports service liveness as JSON.
func HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
}
