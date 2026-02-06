// Example demonstrating go-monitor with gorilla/mux.
//
// Run with: go run .
// Then visit: http://localhost:8080/users/123
//
// You'll see NDJSON events printed to stdout.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/gorilla/mux"
)

var buildVersion = "v0.0.1"

func main() {
	// Initialize the monitor
	err := monitor.Init(monitor.Config{
		Service:   "users",
		Env:       "dev",
		IngestURL: "", // Shipper disabled, stdout only
	})
	if err != nil {
		log.Fatalf("failed to init monitor: %v", err)
	}
	defer monitor.Shutdown()

	// Emit startup event
	monitor.Emit(context.Background(), "service.startup", map[string]any{
		"version": buildVersion,
	})

	// Setup router with middleware
	r := mux.NewRouter()
	r.Use(monitor.Middleware)

	r.HandleFunc("/users/{id}", getUser).Methods("GET")
	r.HandleFunc("/health", healthCheck).Methods("GET")

	fmt.Println("Starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func getUser(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	userID := vars["id"]

	// Emit event - IDs are automatically pulled from context
	monitor.Emit(r.Context(), "user.get", map[string]any{
		"user_id": userID,
	})

	// Simulate response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":   userID,
		"name": "John Doe",
	})
}

func healthCheck(w http.ResponseWriter, r *http.Request) {
	monitor.Emit(r.Context(), "health.check", nil)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
