package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

var startTime = time.Now()

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		fmt.Fprint(w, "OK")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"app":     "s2z-target",
			"status":  "running",
			"uptime":  time.Since(startTime).Round(time.Second).String(),
			"time":    time.Now().UTC().Format(time.RFC3339),
			"method":  r.Method,
			"path":    r.URL.Path,
			"headers": r.Header,
		})
	})

	log.Printf("s2z-target starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
