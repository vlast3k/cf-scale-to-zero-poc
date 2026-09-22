package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "demo-app-1"
	}

	startTime := time.Now()
	color := randomColor()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		uptime := time.Since(startTime).Round(time.Millisecond)
		fmt.Fprintf(w, "Hello from %s! (uptime: %s, color: %s)", appName, uptime, color)
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "OK")
	})

	http.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"app":"%s","uptime_ms":%d,"started":"%s"}`,
			appName, time.Since(startTime).Milliseconds(), startTime.Format(time.RFC3339))
	})

	fmt.Printf("%s listening on port %s\n", appName, port)
	http.ListenAndServe(":"+port, nil)
}

func randomColor() string {
	colors := []string{"blue", "green", "red", "purple", "orange", "teal"}
	return colors[rand.Intn(len(colors))]
}
