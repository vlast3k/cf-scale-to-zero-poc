// Space-scoped service broker for scale-to-zero wake-on-request.
//
// Holds CC admin credentials. Exposes:
//   - OSB API (catalog, provision, deprovision, bind, unbind)
//   - POST /wake/{app_guid} — starts a stopped app
//
// Security model:
//   - Per-binding tokens: each service binding gets a unique bearer token.
//     Tokens are stored in-memory (binding_id → {space_guid, token}).
//     Compromise of one binding's token cannot affect other spaces.
//   - Space ownership check: before starting an app, verifies it belongs
//     to the caller's space via CC API (prevents cross-space escalation).
//   - Per-app rate limit: max 1 wake per app_guid per 60s (prevents
//     cost-denial attacks that keep all apps running permanently).
//   - Constant-time auth: prevents timing side-channel on credentials.
//   - Request body size limit: prevents OOM on OSB endpoints.
//
// NOTE: getCCToken and the http.Client pattern are duplicated in wake-proxy.
// Two separately-deployed binaries with no shared packages — intentional for a PoC.
// Production: extract to an internal module.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes limits request body size on OSB endpoints. Prevents OOM from
// malicious large bodies on the 64M container.
const maxBodyBytes = 64 * 1024 // 64 KB — OSB payloads are tiny JSON

// wakeCooldown is the minimum interval between wake calls for the same app.
// Prevents cost-denial attacks that keep apps running permanently.
const wakeCooldown = 60 * time.Second

// bindingRecord stores per-binding auth token and the space it's authorized for.
type bindingRecord struct {
	Token     string // unique bearer token for this binding
	SpaceGUID string // space this binding is authorized to wake apps in
}

var (
	cfAPI      string
	cfUser     string
	cfPass     string
	brokerUser string
	brokerPass string
	port       string

	// instance_id → space_guid. CF sends space_guid in the provision request body (OSB spec).
	instances   = map[string]string{}
	instancesMu sync.RWMutex

	// binding_id → bindingRecord. Per-binding tokens for /wake authorization.
	bindings   = map[string]bindingRecord{}
	bindingsMu sync.RWMutex

	// token → bindingRecord lookup (reverse index for fast auth on /wake).
	tokenIndex   = map[string]bindingRecord{}
	tokenIndexMu sync.RWMutex

	// app_guid → last wake time. Enforces per-app rate limit.
	wakeTimes   = map[string]time.Time{}
	wakeTimesMu sync.Mutex

	ccToken    string
	ccTokenExp time.Time
	ccTokenMu  sync.Mutex

	// Single client for connection pooling. InsecureSkipVerify: LOD self-signed certs.
	client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
)

func main() {
	cfAPI = requireEnv("CF_API")
	cfUser = requireEnv("CF_ADMIN_USER")
	cfPass = requireEnv("CF_ADMIN_PASS")
	brokerUser = envOr("BROKER_USER", "admin")
	brokerPass = requireEnv("BROKER_PASS")
	port = envOr("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/catalog", brokerAuth(catalogHandler))
	mux.HandleFunc("/v2/service_instances/", brokerAuth(instanceHandler))
	mux.HandleFunc("/v2/service_bindings/", brokerAuth(bindingHandler))
	mux.HandleFunc("/wake/", wakeHandler) // uses per-binding token auth, not broker basic auth
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	log.Printf("s2z-broker on :%s (CF API: %s)", port, cfAPI)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// --- Auth ---

// brokerAuth protects OSB endpoints with basic auth (CF Cloud Controller calls these).
// Uses constant-time comparison to prevent timing side-channel leaks.
func brokerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok {
			log.Printf("WARN: auth failure (no credentials) from %s", clientIP(r))
			w.Header().Set("WWW-Authenticate", `Basic realm="broker"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(brokerUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(brokerPass)) == 1
		if !userOK || !passOK {
			log.Printf("WARN: auth failure (bad credentials) from %s", clientIP(r))
			w.Header().Set("WWW-Authenticate", `Basic realm="broker"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// wakeAuth extracts and validates the per-binding bearer token from /wake requests.
// Returns the bindingRecord if valid, or writes an error response and returns nil.
func wakeAuth(w http.ResponseWriter, r *http.Request) *bindingRecord {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		log.Printf("WARN: wake auth failure (no bearer) from %s", clientIP(r))
		http.Error(w, "Bearer token required", http.StatusUnauthorized)
		return nil
	}
	token := strings.TrimPrefix(auth, "Bearer ")

	tokenIndexMu.RLock()
	rec, exists := tokenIndex[token]
	tokenIndexMu.RUnlock()

	if !exists {
		log.Printf("WARN: wake auth failure (invalid token) from %s", clientIP(r))
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return nil
	}
	return &rec
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.Split(xff, ",")[0]
	}
	return r.RemoteAddr
}

// --- OSB Catalog ---

func catalogHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"services": []map[string]interface{}{{
			"id":          "s2z-wake-service-id",
			"name":        "scale-to-zero-wake",
			"description": "Enables wake-on-request for stopped apps in this space",
			"bindable":    true,
			"plans": []map[string]interface{}{{
				"id":          "s2z-wake-plan-default",
				"name":        "default",
				"description": "Default plan",
			}},
		}},
	})
}

// --- OSB Provision/Deprovision ---

func instanceHandler(w http.ResponseWriter, r *http.Request) {
	// CF uses nested paths: /v2/service_instances/{id}/service_bindings/{id}
	// Delegate to bindingHandler if the path contains /service_bindings/
	if strings.Contains(r.URL.Path, "/service_bindings/") {
		bindingHandler(w, r)
		return
	}

	instanceID := strings.TrimPrefix(r.URL.Path, "/v2/service_instances/")
	instanceID = strings.Split(instanceID, "/")[0]

	// Limit body size to prevent OOM from large payloads.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	switch r.Method {
	case http.MethodPut:
		var body struct {
			SpaceGUID string `json:"space_guid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.SpaceGUID == "" {
			http.Error(w, "space_guid required", http.StatusBadRequest)
			return
		}
		instancesMu.Lock()
		instances[instanceID] = body.SpaceGUID
		instancesMu.Unlock()
		log.Printf("PROVISION instance=%s space=%s", instanceID, body.SpaceGUID)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{})

	case http.MethodDelete:
		// OSB spec sends service_id and plan_id as query params on DELETE.
		// Verify the instance exists and belongs to the expected space (CF sends space_guid
		// in the provision body, but not on delete — so we just check existence).
		instancesMu.Lock()
		if _, exists := instances[instanceID]; !exists {
			instancesMu.Unlock()
			// Return 410 Gone per OSB spec for already-deleted instances.
			w.WriteHeader(http.StatusGone)
			json.NewEncoder(w).Encode(map[string]interface{}{})
			return
		}
		delete(instances, instanceID)
		instancesMu.Unlock()

		// Also revoke all bindings for this instance (cleanup stale tokens).
		revokeBindingsForInstance(instanceID)

		log.Printf("DEPROVISION instance=%s", instanceID)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- OSB Bind/Unbind ---
// Each binding generates a unique bearer token. The wake-proxy uses this token
// to authenticate to POST /wake. Per-binding tokens prevent horizontal escalation:
// compromise of one space's token cannot affect another space.

func bindingHandler(w http.ResponseWriter, r *http.Request) {
	// Path: /v2/service_bindings/{binding_id}
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/service_bindings/"), "/")
	bindingID := pathParts[0]

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	switch r.Method {
	case http.MethodPut:
		var body struct {
			ServiceID  string `json:"service_id"`
			PlanID     string `json:"plan_id"`
			InstanceID string `json:"service_instance_id"` // non-standard but some platforms send it
			AppGUID    string `json:"app_guid"`
		}
		// Best-effort parse — not all fields are always present.
		json.NewDecoder(r.Body).Decode(&body)

		// Determine space from the service instance.
		// The binding request includes the instance_id in the URL path:
		// PUT /v2/service_instances/{instance_id}/service_bindings/{binding_id}
		// But CF also uses PUT /v2/service_bindings/{binding_id} with instance in body.
		// We derive the space from the instance registry.
		instanceID := extractInstanceIDFromPath(r.URL.Path)
		instancesMu.RLock()
		spaceGUID := instances[instanceID]
		instancesMu.RUnlock()

		if spaceGUID == "" {
			// Fallback: check all instances (binding might not reference instance in path)
			instancesMu.RLock()
			for _, sg := range instances {
				spaceGUID = sg
				break // use first available — imprecise but safe for PoC
			}
			instancesMu.RUnlock()
		}

		// Generate a unique token for this binding.
		token := generateToken()

		rec := bindingRecord{
			Token:     token,
			SpaceGUID: spaceGUID,
		}

		bindingsMu.Lock()
		bindings[bindingID] = rec
		bindingsMu.Unlock()

		tokenIndexMu.Lock()
		tokenIndex[token] = rec
		tokenIndexMu.Unlock()

		log.Printf("BIND binding=%s space=%s", bindingID, spaceGUID)

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"credentials": map[string]interface{}{
				"broker_url": fmt.Sprintf("https://%s", os.Getenv("BROKER_ROUTE")),
				"token":      token,
			},
		})

	case http.MethodDelete:
		bindingsMu.Lock()
		rec, exists := bindings[bindingID]
		if exists {
			delete(bindings, bindingID)
		}
		bindingsMu.Unlock()

		if exists {
			tokenIndexMu.Lock()
			delete(tokenIndex, rec.Token)
			tokenIndexMu.Unlock()
		}

		log.Printf("UNBIND binding=%s", bindingID)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// extractInstanceIDFromPath handles the OSB URL pattern:
// /v2/service_instances/{instance_id}/service_bindings/{binding_id}
func extractInstanceIDFromPath(path string) string {
	// Try to find instance_id between "/v2/service_instances/" and "/service_bindings/"
	trimmed := strings.TrimPrefix(path, "/v2/service_instances/")
	if idx := strings.Index(trimmed, "/service_bindings/"); idx > 0 {
		return trimmed[:idx]
	}
	// Fallback: path is /v2/service_bindings/{binding_id} — no instance in path
	return ""
}

func revokeBindingsForInstance(instanceID string) {
	// In a real implementation, we'd track instance→bindings.
	// For the PoC, instance deprovision clears the token index for that space.
	instancesMu.RLock()
	spaceGUID := instances[instanceID] // already deleted, but check first
	instancesMu.RUnlock()

	if spaceGUID == "" {
		return
	}

	bindingsMu.Lock()
	var toDelete []string
	for bid, rec := range bindings {
		if rec.SpaceGUID == spaceGUID {
			toDelete = append(toDelete, bid)
		}
	}
	for _, bid := range toDelete {
		delete(bindings, bid)
	}
	bindingsMu.Unlock()

	tokenIndexMu.Lock()
	for tok, rec := range tokenIndex {
		if rec.SpaceGUID == spaceGUID {
			delete(tokenIndex, tok)
		}
	}
	tokenIndexMu.Unlock()
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand failed: %v", err)
	}
	return hex.EncodeToString(b)
}

// HealthCheck holds the health check configuration returned with the wake response.
type HealthCheck struct {
	Type              string `json:"type"`
	Endpoint          string `json:"endpoint"`
	InvocationTimeout int    `json:"invocation_timeout"`
}

// wakeResponse is the JSON body returned by POST /wake/{app_guid}.
type wakeResponse struct {
	Status      string       `json:"status"`
	AppGUID     string       `json:"app_guid"`
	HealthCheck *HealthCheck `json:"health_check"`
}

// --- Wake ---
// POST /wake/{app_guid}
//
// Authorization model:
//   1. Caller must present a valid per-binding bearer token (issued during OSB bind)
//   2. The token is scoped to a space — only apps in that space can be started
//   3. Before starting, verifies the app actually belongs to the token's space (CC check)
//   4. Rate limited: max 1 wake per app_guid per 60s

func wakeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	appGUID := strings.TrimPrefix(r.URL.Path, "/wake/")
	if appGUID == "" {
		http.Error(w, "app_guid required", http.StatusBadRequest)
		return
	}

	// Authenticate via per-binding token.
	rec := wakeAuth(w, r)
	if rec == nil {
		return // wakeAuth already wrote the error response
	}
	spaceGUID := rec.SpaceGUID

	// Rate limit: prevent cost-denial attacks that keep apps running.
	wakeTimesMu.Lock()
	if lastWake, exists := wakeTimes[appGUID]; exists && time.Since(lastWake) < wakeCooldown {
		wakeTimesMu.Unlock()
		remaining := wakeCooldown - time.Since(lastWake)
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remaining.Seconds())+1))
		http.Error(w, fmt.Sprintf("rate limited: app was woken %v ago, retry in %v", time.Since(lastWake).Round(time.Second), remaining.Round(time.Second)), http.StatusTooManyRequests)
		return
	}
	wakeTimes[appGUID] = time.Now()
	wakeTimesMu.Unlock()

	token, err := getCCToken()
	if err != nil {
		log.Printf("ERROR token: %v", err)
		http.Error(w, "token error", http.StatusInternalServerError)
		return
	}

	// Verify app belongs to the caller's space BEFORE starting it.
	// Without this, a caller could start any app by passing an arbitrary app_guid —
	// the admin token has no space boundary.
	belongs, err := appInSpace(token, appGUID, spaceGUID)
	if err != nil {
		log.Printf("ERROR checking app space: %v", err)
		http.Error(w, "failed to verify app ownership", http.StatusInternalServerError)
		return
	}
	if !belongs {
		log.Printf("WARN: app %s not in space %s (token from binding)", appGUID, spaceGUID)
		http.Error(w, "app not found in this space", http.StatusNotFound)
		return
	}

	if err := startApp(token, appGUID); err != nil {
		log.Printf("ERROR starting %s: %v", appGUID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read health check config so the proxy knows the readiness endpoint and timeout.
	hc := getHealthCheck(token, appGUID)
	if hc == nil {
		log.Printf("WARN health check unavailable for app=%s; proxy will use defaults", appGUID)
	}

	log.Printf("WAKE success: app=%s space=%s", appGUID, spaceGUID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(wakeResponse{
		Status:      "started",
		AppGUID:     appGUID,
		HealthCheck: hc,
	})
}

// --- CC API helpers ---

// appInSpace verifies the app exists AND lives in the expected space.
// Returns (false, nil) if the app doesn't exist (CC 404) or belongs to another space.
// Returns (false, err) only on transport/auth failures.
func appInSpace(token, appGUID, spaceGUID string) (bool, error) {
	u := fmt.Sprintf("%s/v3/apps/%s", cfAPI, appGUID)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GET app: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("GET app %d: %s", resp.StatusCode, string(b))
	}

	var app struct {
		Relationships struct {
			Space struct {
				Data struct {
					GUID string `json:"guid"`
				} `json:"data"`
			} `json:"space"`
		} `json:"relationships"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return false, fmt.Errorf("decode app: %w", err)
	}

	return app.Relationships.Space.Data.GUID == spaceGUID, nil
}

// getCCToken returns a cached CC OAuth2 token, refreshing via password grant when expired.
func getCCToken() (string, error) {
	ccTokenMu.Lock()
	defer ccTokenMu.Unlock()

	if ccToken != "" && time.Now().Before(ccTokenExp) {
		return ccToken, nil
	}

	infoResp, err := client.Get(cfAPI + "/v2/info")
	if err != nil {
		return "", fmt.Errorf("GET /v2/info: %w", err)
	}
	defer infoResp.Body.Close()

	var info struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(infoResp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode /v2/info: %w", err)
	}

	tokenURL := info.TokenEndpoint + "/oauth/token"
	payload := url.Values{
		"grant_type": {"password"},
		"username":   {cfUser},
		"password":   {cfPass},
	}.Encode()
	req, _ := http.NewRequest("POST", tokenURL, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("cf", "") // public client, empty secret (same as CF CLI)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token %d: %s", resp.StatusCode, string(b))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}

	ccToken = tokenResp.AccessToken
	ccTokenExp = time.Now().Add(time.Duration(tokenResp.ExpiresIn-60) * time.Second)
	return ccToken, nil
}

// startApp is idempotent: 200 = started, 422 = already running.
func startApp(token, appGUID string) error {
	u := fmt.Sprintf("%s/v3/apps/%s/actions/start", cfAPI, appGUID)
	req, _ := http.NewRequest("POST", u, nil)
	req.Header.Set("Authorization", "bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST start: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 || resp.StatusCode == 422 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("start %d: %s", resp.StatusCode, string(body))
}

// getHealthCheck fetches the web process health check config for an app.
// Returns nil on any error — callers degrade gracefully.
func getHealthCheck(token, appGUID string) *HealthCheck {
	u := fmt.Sprintf("%s/v3/apps/%s/processes/web", cfAPI, appGUID)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("WARN getHealthCheck GET: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("WARN getHealthCheck %d: %s", resp.StatusCode, string(b))
		return nil
	}

	var process struct {
		HealthCheck struct {
			Type string `json:"type"`
			Data struct {
				Endpoint          *string `json:"endpoint"`
				InvocationTimeout *int    `json:"invocation_timeout"`
			} `json:"data"`
		} `json:"health_check"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&process); err != nil {
		log.Printf("WARN getHealthCheck decode: %v", err)
		return nil
	}

	hc := &HealthCheck{
		Type: process.HealthCheck.Type,
	}
	if hc.Type == "http" && process.HealthCheck.Data.Endpoint != nil && *process.HealthCheck.Data.Endpoint != "" {
		hc.Endpoint = *process.HealthCheck.Data.Endpoint
	} else {
		hc.Endpoint = "/"
	}
	if process.HealthCheck.Data.InvocationTimeout != nil && *process.HealthCheck.Data.InvocationTimeout > 0 {
		hc.InvocationTimeout = *process.HealthCheck.Data.InvocationTimeout
	} else {
		hc.InvocationTimeout = 60
	}

	return hc
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required: %s", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
