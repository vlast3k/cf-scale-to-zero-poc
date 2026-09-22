package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// =============================================================================
// Token Manager (client_credentials grant, same as route-service proxy)
// =============================================================================

type tokenManager struct {
	mu          sync.RWMutex
	accessToken string
	expiresAt   time.Time
	tokenURL    string
	clientID    string
	clientSecret string
	cfAPI       string
}

func newTokenManager(cfAPI, clientID, clientSecret string) *tokenManager {
	return &tokenManager{
		clientID:     clientID,
		clientSecret: clientSecret,
		cfAPI:        cfAPI,
	}
}

func (tm *tokenManager) Token() string {
	tm.mu.RLock()
	token := tm.accessToken
	expires := tm.expiresAt
	tm.mu.RUnlock()

	if time.Until(expires) < 60*time.Second {
		if err := tm.Refresh(); err != nil {
			log.Printf("WARN: proactive token refresh failed: %v", err)
		} else {
			tm.mu.RLock()
			token = tm.accessToken
			tm.mu.RUnlock()
		}
	}
	return token
}

func (tm *tokenManager) Refresh() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.tokenURL == "" {
		tokenURL, err := tm.discoverTokenEndpoint()
		if err != nil {
			return fmt.Errorf("discovering UAA endpoint: %w", err)
		}
		tm.tokenURL = tokenURL
		log.Printf("UAA token endpoint: %s", tm.tokenURL)
	}

	body := "grant_type=client_credentials"
	authHeader := "Basic " + base64.StdEncoding.EncodeToString(
		[]byte(tm.clientID+":"+tm.clientSecret))

	req, err := http.NewRequest("POST", tm.tokenURL, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("UAA returned %d: %s", resp.StatusCode, string(respBody))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("UAA returned empty access_token")
	}

	tm.accessToken = tokenResp.TokenType + " " + tokenResp.AccessToken
	tm.expiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second * 80 / 100)

	log.Printf("Token refreshed (expires_in=%ds, next refresh in %s)",
		tokenResp.ExpiresIn, time.Until(tm.expiresAt).Round(time.Second))
	return nil
}

func (tm *tokenManager) discoverTokenEndpoint() (string, error) {
	resp, err := httpClient.Get(tm.cfAPI + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var info struct {
		Links struct {
			UAA   struct{ Href string `json:"href"` } `json:"uaa"`
			Login struct{ Href string `json:"href"` } `json:"login"`
		} `json:"links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", err
	}

	base := info.Links.UAA.Href
	if base == "" {
		base = info.Links.Login.Href
	}
	if base == "" {
		return "", fmt.Errorf("CF API / returned no UAA or login link")
	}
	return base + "/oauth/token", nil
}

func (tm *tokenManager) backgroundRefresh() {
	for {
		tm.mu.RLock()
		sleepDur := time.Until(tm.expiresAt) - 30*time.Second
		tm.mu.RUnlock()

		if sleepDur < 10*time.Second {
			sleepDur = 10 * time.Second
		}
		time.Sleep(sleepDur)

		if err := tm.Refresh(); err != nil {
			log.Printf("ERROR: background token refresh failed: %v", err)
			time.Sleep(30 * time.Second)
		}
	}
}

// =============================================================================
// Managed Endpoint
// =============================================================================

type EndpointState string

const (
	// CF API states (real, from GET /v3/apps/{guid})
	StateStopped EndpointState = "STOPPED"
	StateStarted EndpointState = "STARTED"
	StateUnknown EndpointState = "UNKNOWN"

	// Coordinator-observed states (honest labels, not CF states)
	StateInvoking  EndpointState = "INVOKING"  // CF API accepted start, waiting for backend
	StateStopping  EndpointState = "STOPPING"  // CF API accepted stop, waiting for confirmation
	StateReachable EndpointState = "REACHABLE" // Backend HTTP probe succeeded (app serving traffic)
)

type ManagedEndpoint struct {
	Name       string        `json:"name"`
	GUID       string        `json:"guid"`
	BackendURL string        `json:"backend_url"`
	State      EndpointState `json:"state"`
	mu         sync.Mutex    // protects State during invoke/stop operations
	invoking   bool          // true while an invoke is in-flight; monitoring skips this endpoint
}

type InvokeResult struct {
	Endpoint     string        `json:"endpoint"`
	StatusCode   int           `json:"status_code"`
	ResponseBody string        `json:"response_body"`
	DurationMs   float64       `json:"duration_ms"`
	Error        string        `json:"error,omitempty"`
	Timestamp    string        `json:"timestamp"`
}

// =============================================================================
// WebSocket Hub — broadcasts state to all connected browsers
// =============================================================================

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type wsHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

func newWSHub() *wsHub {
	return &wsHub{clients: make(map[*websocket.Conn]bool)}
}

func (h *wsHub) add(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()
}

func (h *wsHub) remove(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
}

func (h *wsHub) broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.clients {
		conn.WriteMessage(websocket.TextMessage, data)
	}
}

// =============================================================================
// Billing Event Tracker — CF Usage Events (GB-seconds measurement)
// =============================================================================

type UsageEvent struct {
	GUID      string `json:"guid"`
	CreatedAt string `json:"created_at"`
	State     struct {
		Current  string `json:"current"`
		Previous string `json:"previous"`
	} `json:"state"`
	App struct {
		GUID string `json:"guid"`
		Name string `json:"name"`
	} `json:"app"`
	MemoryInMBPerInstance struct {
		Current  int `json:"current"`
		Previous int `json:"previous"`
	} `json:"memory_in_mb_per_instance"`
	InstanceCount struct {
		Current  int `json:"current"`
		Previous int `json:"previous"`
	} `json:"instance_count"`
}

type BillingWindow struct {
	AppName    string    `json:"app_name"`
	AppGUID    string    `json:"app_guid"`
	StartedAt  time.Time `json:"started_at"`
	MemoryMB   int       `json:"memory_mb"`
	Instances  int       `json:"instances"`
}

type BillingTracker struct {
	mu            sync.Mutex
	watchedGUIDs  map[string]string  // app GUID → app name
	openWindows   map[string]*BillingWindow // app GUID → active billing window
	totalGBSec    map[string]float64 // app GUID → accumulated GB-seconds (closed windows)
	lastEventGUID string             // cursor for incremental polling
	events        []BillingLogEntry  // recent events for UI display
}

type BillingLogEntry struct {
	Timestamp string  `json:"timestamp"`
	AppName   string  `json:"app_name"`
	EventType string  `json:"event_type"` // "STARTED", "STOPPED", "SCALED"
	MemoryMB  int     `json:"memory_mb"`
	Instances int     `json:"instances"`
	GBSeconds float64 `json:"gb_seconds"` // accumulated at time of event
}

type BillingStatus struct {
	AppName       string  `json:"app_name"`
	AppGUID       string  `json:"app_guid"`
	Running       bool    `json:"running"`
	MemoryMB      int     `json:"memory_mb"`
	Instances     int     `json:"instances"`
	GBSeconds     float64 `json:"gb_seconds"`     // total accumulated (closed + open)
	RunningFor    string  `json:"running_for"`     // duration string if running
}

func newBillingTracker(endpoints []*ManagedEndpoint) *BillingTracker {
	watched := make(map[string]string)
	for _, ep := range endpoints {
		watched[ep.GUID] = ep.Name
	}
	return &BillingTracker{
		watchedGUIDs: watched,
		openWindows:  make(map[string]*BillingWindow),
		totalGBSec:   make(map[string]float64),
	}
}

func (bt *BillingTracker) GetStatus() []BillingStatus {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	var statuses []BillingStatus
	for guid, name := range bt.watchedGUIDs {
		s := BillingStatus{
			AppName: name,
			AppGUID: guid,
		}
		s.GBSeconds = bt.totalGBSec[guid]

		if w := bt.openWindows[guid]; w != nil {
			s.Running = true
			s.MemoryMB = w.MemoryMB
			s.Instances = w.Instances
			elapsed := time.Since(w.StartedAt).Seconds()
			s.GBSeconds += (float64(w.MemoryMB) / 1024.0) * float64(w.Instances) * elapsed
			s.RunningFor = time.Since(w.StartedAt).Round(100 * time.Millisecond).String()
		}
		statuses = append(statuses, s)
	}
	return statuses
}

func (bt *BillingTracker) GetRecentEvents() []BillingLogEntry {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	// Return last 20 events
	if len(bt.events) > 20 {
		return bt.events[len(bt.events)-20:]
	}
	return bt.events
}

func (bt *BillingTracker) processEvent(ev UsageEvent) {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	name := bt.watchedGUIDs[ev.App.GUID]
	ts, _ := time.Parse(time.RFC3339, ev.CreatedAt)

	switch ev.State.Current {
	case "STARTED":
		// Close existing window if any (handles scale events: STARTED→STARTED)
		if w := bt.openWindows[ev.App.GUID]; w != nil {
			elapsed := ts.Sub(w.StartedAt).Seconds()
			gbSec := (float64(w.MemoryMB) / 1024.0) * float64(w.Instances) * elapsed
			bt.totalGBSec[ev.App.GUID] += gbSec
		}

		// Open new billing window
		bt.openWindows[ev.App.GUID] = &BillingWindow{
			AppName:   name,
			AppGUID:   ev.App.GUID,
			StartedAt: ts,
			MemoryMB:  ev.MemoryInMBPerInstance.Current,
			Instances: ev.InstanceCount.Current,
		}

		eventType := "STARTED"
		if ev.State.Previous == "STARTED" {
			eventType = "SCALED"
		}

		bt.events = append(bt.events, BillingLogEntry{
			Timestamp: ev.CreatedAt,
			AppName:   name,
			EventType: eventType,
			MemoryMB:  ev.MemoryInMBPerInstance.Current,
			Instances: ev.InstanceCount.Current,
			GBSeconds: bt.totalGBSec[ev.App.GUID],
		})

	case "STOPPED":
		// Close billing window
		if w := bt.openWindows[ev.App.GUID]; w != nil {
			elapsed := ts.Sub(w.StartedAt).Seconds()
			gbSec := (float64(w.MemoryMB) / 1024.0) * float64(w.Instances) * elapsed
			bt.totalGBSec[ev.App.GUID] += gbSec
			delete(bt.openWindows, ev.App.GUID)
		}

		bt.events = append(bt.events, BillingLogEntry{
			Timestamp: ev.CreatedAt,
			AppName:   name,
			EventType: "STOPPED",
			MemoryMB:  ev.MemoryInMBPerInstance.Current,
			Instances: ev.InstanceCount.Current,
			GBSeconds: bt.totalGBSec[ev.App.GUID],
		})
	}

	// Keep max 50 events in memory
	if len(bt.events) > 50 {
		bt.events = bt.events[len(bt.events)-50:]
	}
}

func (bt *BillingTracker) pollEvents() {
	reqURL := tokens.cfAPI + "/v3/app_usage_events?order_by=-created_at&per_page=50"
	if bt.lastEventGUID != "" {
		reqURL = tokens.cfAPI + "/v3/app_usage_events?per_page=100&after_guid=" + bt.lastEventGUID
	}

	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("Authorization", tokens.Token())

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("WARN: billing event poll failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		tokens.Refresh()
		req, _ = http.NewRequest("GET", reqURL, nil)
		req.Header.Set("Authorization", tokens.Token())
		resp, err = httpClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != 200 {
		return
	}

	var result struct {
		Resources []UsageEvent `json:"resources"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	// Process events (they come in chronological order with after_guid)
	// For first poll (order_by=-created_at), they come newest-first — reverse
	var toProcess []UsageEvent
	if bt.lastEventGUID == "" {
		// First poll: reverse to get chronological, only keep our apps
		for i := len(result.Resources) - 1; i >= 0; i-- {
			ev := result.Resources[i]
			if _, watched := bt.watchedGUIDs[ev.App.GUID]; watched {
				toProcess = append(toProcess, ev)
			}
		}
	} else {
		// Incremental: already chronological
		for _, ev := range result.Resources {
			if _, watched := bt.watchedGUIDs[ev.App.GUID]; watched {
				toProcess = append(toProcess, ev)
			}
		}
	}

	for _, ev := range toProcess {
		// Skip non-billable events
		if ev.State.Current != "STARTED" && ev.State.Current != "STOPPED" {
			continue
		}
		bt.processEvent(ev)
		log.Printf("📊 Billing event: %s %s→%s (%s, %dMB×%d)",
			bt.watchedGUIDs[ev.App.GUID], ev.State.Previous, ev.State.Current,
			ev.CreatedAt, ev.MemoryInMBPerInstance.Current, ev.InstanceCount.Current)

		// Broadcast to UI
		hub.broadcast(map[string]interface{}{
			"type": "billing_event",
			"event": BillingLogEntry{
				Timestamp: ev.CreatedAt,
				AppName:   bt.watchedGUIDs[ev.App.GUID],
				EventType: ev.State.Current,
				MemoryMB:  ev.MemoryInMBPerInstance.Current,
				Instances: ev.InstanceCount.Current,
				GBSeconds: bt.totalGBSec[ev.App.GUID],
			},
		})

		// Immediately update billing cards (so STOPPED events clear the active state)
		hub.broadcast(map[string]interface{}{
			"type":     "billing_update",
			"statuses": bt.GetStatus(),
		})
	}

	// Update cursor
	if len(result.Resources) > 0 {
		// Last resource in the response is the newest (for after_guid) or first (for order_by)
		if bt.lastEventGUID == "" {
			bt.lastEventGUID = result.Resources[0].GUID // newest from first poll
		} else if len(result.Resources) > 0 {
			bt.lastEventGUID = result.Resources[len(result.Resources)-1].GUID
		}
	}
}

func (bt *BillingTracker) backgroundPoll() {
	// Seed billing state from current CF API state (handles apps already running at startup)
	bt.seedFromCurrentState()

	// Initial poll to set the cursor
	bt.pollEvents()
	log.Printf("📊 Billing tracker started (watching %d apps)", len(bt.watchedGUIDs))

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Also broadcast GB-seconds counter updates every second for running apps
	counterTicker := time.NewTicker(1 * time.Second)
	defer counterTicker.Stop()

	for {
		select {
		case <-ticker.C:
			bt.pollEvents()
		case <-counterTicker.C:
			statuses := bt.GetStatus()
			// Only broadcast if any app is running (avoid noise)
			anyRunning := false
			for _, s := range statuses {
				if s.Running {
					anyRunning = true
					break
				}
			}
			if anyRunning {
				hub.broadcast(map[string]interface{}{
					"type":     "billing_update",
					"statuses": statuses,
				})
			}
		}
	}
}

// seedFromCurrentState checks CF API for already-running apps and opens billing windows
func (bt *BillingTracker) seedFromCurrentState() {
	for guid, name := range bt.watchedGUIDs {
		state, err := getAppState(guid)
		if err != nil {
			continue
		}
		if state == "STARTED" {
			bt.mu.Lock()
			bt.openWindows[guid] = &BillingWindow{
				AppName:   name,
				AppGUID:   guid,
				StartedAt: time.Now(), // approximate — we don't know exact start time
				MemoryMB:  32,         // default for demo apps
				Instances: 1,
			}
			bt.mu.Unlock()
			log.Printf("📊 Seeded billing window for %s (already running)", name)
		}
	}
}

// Reset clears all billing state (for clean demo starts)
func (bt *BillingTracker) Reset() {
	bt.mu.Lock()
	bt.openWindows = make(map[string]*BillingWindow)
	bt.totalGBSec = make(map[string]float64)
	bt.events = nil
	bt.mu.Unlock()
	log.Printf("📊 Billing counters reset")
}

var billing *BillingTracker

// =============================================================================
// Global State
// =============================================================================

var (
	tokens     *tokenManager
	httpClient *http.Client
	endpoints  []*ManagedEndpoint
	hub        *wsHub
	monitoring bool
	monMu      sync.Mutex
)

// =============================================================================
// CF API Helpers
// =============================================================================

func getAppState(guid string) (string, error) {
	reqURL := fmt.Sprintf("%s/v3/apps/%s", tokens.cfAPI, guid)
	req, _ := http.NewRequest("GET", reqURL, nil)
	req.Header.Set("Authorization", tokens.Token())

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		if err := tokens.Refresh(); err != nil {
			return "", err
		}
		req, _ = http.NewRequest("GET", reqURL, nil)
		req.Header.Set("Authorization", tokens.Token())
		resp, err = httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
	}

	var result struct {
		State string `json:"state"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return result.State, nil
}

func cfAPIPost(path string) error {
	reqURL := tokens.cfAPI + path
	req, _ := http.NewRequest("POST", reqURL, nil)
	req.Header.Set("Authorization", tokens.Token())

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 401 {
		if err := tokens.Refresh(); err != nil {
			return fmt.Errorf("token refresh failed: %w", err)
		}
		req, _ = http.NewRequest("POST", reqURL, nil)
		req.Header.Set("Authorization", tokens.Token())
		resp, err = httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API %s returned %d: %s", path, resp.StatusCode, string(body))
	}
	return nil
}

func cfAPIStop(guid string) error {
	return cfAPIPost(fmt.Sprintf("/v3/apps/%s/actions/stop", guid))
}

func cfAPIStart(guid string) error {
	return cfAPIPost(fmt.Sprintf("/v3/apps/%s/actions/start", guid))
}

// =============================================================================
// Endpoint Operations
// =============================================================================

func refreshEndpointStates() {
	for _, ep := range endpoints {
		ep.mu.Lock()
		if ep.invoking {
			// Don't overwrite state while an invoke/stop operation is in-flight.
			// The operation goroutine owns the state until it completes.
			ep.mu.Unlock()
			continue
		}
		ep.mu.Unlock()

		state, err := getAppState(ep.GUID)
		if err != nil {
			ep.mu.Lock()
			if !ep.invoking { // re-check after API call
				ep.State = StateUnknown
			}
			ep.mu.Unlock()
		} else {
			ep.mu.Lock()
			if !ep.invoking {
				ep.State = EndpointState(state)
			}
			ep.mu.Unlock()
		}
	}
}

func invokeEndpoint(ep *ManagedEndpoint) InvokeResult {
	start := time.Now()

	// Mark endpoint as in-flight — monitoring will skip it
	ep.mu.Lock()
	ep.invoking = true
	ep.mu.Unlock()
	defer func() {
		ep.mu.Lock()
		ep.invoking = false
		ep.mu.Unlock()
	}()

	// First check state and start if needed
	state, err := getAppState(ep.GUID)
	if err != nil {
		return InvokeResult{
			Endpoint:   ep.Name,
			Error:      fmt.Sprintf("getAppState failed: %v", err),
			Timestamp:  time.Now().Format(time.RFC3339Nano),
			DurationMs: float64(time.Since(start).Milliseconds()),
		}
	}

	if state == "STOPPED" {
		// Call CF API first — only broadcast state AFTER it succeeds
		if err := cfAPIStart(ep.GUID); err != nil {
			return InvokeResult{
				Endpoint:   ep.Name,
				Error:      fmt.Sprintf("start failed: %v", err),
				Timestamp:  time.Now().Format(time.RFC3339Nano),
				DurationMs: float64(time.Since(start).Milliseconds()),
			}
		}

		// CF API accepted the start — NOW we can honestly say "INVOKING"
		// (CC has created the DesiredLRP, Diego is scheduling the container)
		ep.mu.Lock()
		ep.State = StateInvoking
		ep.mu.Unlock()
		hub.broadcast(map[string]interface{}{
			"type":     "state_change",
			"endpoint": ep.Name,
			"state":    "INVOKING",
		})
	}

	// Probe backend with retry (handles gorouter convergence)
	probeClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var lastResp *http.Response
	var lastErr error
	maxAttempts := 50 // 100ms * 50 = 5s probe budget

	// Fast probe loop (100ms interval)
	for i := 0; i < maxAttempts; i++ {
		resp, err := probeClient.Get(ep.BackendURL)
		if err == nil {
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode < 500 {
				// Got a real response from the app — this is REACHABLE
				// (verified: traffic flows through gorouter to the app container)
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				ep.mu.Lock()
				ep.State = StateReachable
				ep.mu.Unlock()

				hub.broadcast(map[string]interface{}{
					"type":     "state_change",
					"endpoint": ep.Name,
					"state":    "REACHABLE",
				})

				return InvokeResult{
					Endpoint:     ep.Name,
					StatusCode:   resp.StatusCode,
					ResponseBody: string(bodyBytes),
					DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
					Timestamp:    time.Now().Format(time.RFC3339Nano),
				}
			}
			resp.Body.Close()
			lastResp = resp
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Exponential backoff retries for gorouter divergence
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 5; attempt++ {
		resp, err := probeClient.Get(ep.BackendURL)
		if err == nil {
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode < 500 {
				bodyBytes, _ := io.ReadAll(resp.Body)
				resp.Body.Close()

				ep.mu.Lock()
				ep.State = StateReachable
				ep.mu.Unlock()

				hub.broadcast(map[string]interface{}{
					"type":     "state_change",
					"endpoint": ep.Name,
					"state":    "REACHABLE",
				})

				return InvokeResult{
					Endpoint:     ep.Name,
					StatusCode:   resp.StatusCode,
					ResponseBody: string(bodyBytes),
					DurationMs:   float64(time.Since(start).Microseconds()) / 1000.0,
					Timestamp:    time.Now().Format(time.RFC3339Nano),
				}
			}
			resp.Body.Close()
			lastResp = resp
		} else {
			lastErr = err
		}
		time.Sleep(backoff)
		backoff *= 2
	}

	errMsg := "backend not reachable within timeout"
	if lastErr != nil {
		errMsg = lastErr.Error()
	} else if lastResp != nil {
		errMsg = fmt.Sprintf("last status: %d", lastResp.StatusCode)
	}

	return InvokeResult{
		Endpoint:   ep.Name,
		Error:      errMsg,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000.0,
		Timestamp:  time.Now().Format(time.RFC3339Nano),
	}
}

// =============================================================================
// HTTP Handlers
// =============================================================================

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	hub.add(conn)
	defer func() {
		hub.remove(conn)
		conn.Close()
	}()

	// Send initial state
	refreshEndpointStates()
	conn.WriteJSON(map[string]interface{}{
		"type":           "initial_state",
		"endpoints":      endpoints,
		"billing":        billing.GetStatus(),
		"billing_events": billing.GetRecentEvents(),
	})

	// Keep connection alive — read messages (commands from browser)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var cmd struct {
			Action   string `json:"action"`
			Endpoint string `json:"endpoint"`
		}
		if err := json.Unmarshal(msg, &cmd); err != nil {
			continue
		}

		switch cmd.Action {
		case "invoke":
			go handleInvoke(cmd.Endpoint)
		case "stop":
			go handleStop(cmd.Endpoint)
		case "start_monitoring":
			go startMonitoring()
		case "stop_monitoring":
			stopMonitoring()
		case "reset_demo":
			go handleResetDemo()
		case "refresh":
			refreshEndpointStates()
			conn.WriteJSON(map[string]interface{}{
				"type":      "state_update",
				"endpoints": endpoints,
			})
		}
	}
}

func handleInvoke(name string) {
	var ep *ManagedEndpoint
	for _, e := range endpoints {
		if e.Name == name {
			ep = e
			break
		}
	}
	if ep == nil {
		return
	}

	// Broadcast that we're starting the invoke (with timer_start signal)
	hub.broadcast(map[string]interface{}{
		"type":     "invoke_start",
		"endpoint": name,
	})

	result := invokeEndpoint(ep)

	hub.broadcast(map[string]interface{}{
		"type":   "invoke_result",
		"result": result,
	})
}

func handleStop(name string) {
	var ep *ManagedEndpoint
	for _, e := range endpoints {
		if e.Name == name {
			ep = e
			break
		}
	}
	if ep == nil {
		return
	}

	// Mark as in-flight so monitoring doesn't override
	ep.mu.Lock()
	ep.invoking = true
	ep.mu.Unlock()
	defer func() {
		ep.mu.Lock()
		ep.invoking = false
		ep.mu.Unlock()
	}()

	// Call CF API first — only broadcast AFTER it succeeds
	if err := cfAPIStop(ep.GUID); err != nil {
		hub.broadcast(map[string]interface{}{
			"type":     "error",
			"endpoint": name,
			"error":    err.Error(),
		})
		return
	}

	// CF API confirmed the stop — now broadcast the real state
	ep.mu.Lock()
	ep.State = StateStopped
	ep.mu.Unlock()
	hub.broadcast(map[string]interface{}{
		"type":     "state_change",
		"endpoint": name,
		"state":    "STOPPED",
	})
}

func handleResetDemo() {
	// Stop all running apps
	for _, ep := range endpoints {
		state, _ := getAppState(ep.GUID)
		if state == "STARTED" {
			ep.mu.Lock()
			ep.invoking = true
			ep.mu.Unlock()

			if err := cfAPIStop(ep.GUID); err == nil {
				ep.mu.Lock()
				ep.State = StateStopped
				ep.mu.Unlock()
				hub.broadcast(map[string]interface{}{
					"type":     "state_change",
					"endpoint": ep.Name,
					"state":    "STOPPED",
				})
			}

			ep.mu.Lock()
			ep.invoking = false
			ep.mu.Unlock()
		}
	}

	// Reset billing counters
	billing.Reset()

	// Broadcast clean state
	hub.broadcast(map[string]interface{}{
		"type":     "billing_reset",
		"statuses": billing.GetStatus(),
	})

	log.Printf("🔄 Demo reset: all apps stopped, billing counters cleared")
}

func startMonitoring() {
	monMu.Lock()
	if monitoring {
		monMu.Unlock()
		return
	}
	monitoring = true
	monMu.Unlock()

	hub.broadcast(map[string]interface{}{
		"type":       "monitoring",
		"monitoring": true,
	})

	for {
		monMu.Lock()
		if !monitoring {
			monMu.Unlock()
			return
		}
		monMu.Unlock()

		refreshEndpointStates()
		hub.broadcast(map[string]interface{}{
			"type":      "state_update",
			"endpoints": endpoints,
		})
		time.Sleep(2 * time.Second)
	}
}

func stopMonitoring() {
	monMu.Lock()
	monitoring = false
	monMu.Unlock()

	hub.broadcast(map[string]interface{}{
		"type":       "monitoring",
		"monitoring": false,
	})
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx/gorouter buffering

	// Send initial state
	refreshEndpointStates()
	data, _ := json.Marshal(map[string]interface{}{
		"type":      "initial_state",
		"endpoints": endpoints,
	})
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// Poll and send updates
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			refreshEndpointStates()
			data, _ := json.Marshal(map[string]interface{}{
				"type":      "state_update",
				"endpoints": endpoints,
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "OK")
}

func apiInvokeHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("endpoint")
	if name == "" {
		http.Error(w, "?endpoint= required", http.StatusBadRequest)
		return
	}
	go handleInvoke(name)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "invoking"})
}

func apiStopHandler(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("endpoint")
	if name == "" {
		http.Error(w, "?endpoint= required", http.StatusBadRequest)
		return
	}
	go handleStop(name)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "stopping"})
}

// =============================================================================
// Main
// =============================================================================

func main() {
	cfAPI := mustEnv("CF_API")
	clientID := mustEnv("CF_CLIENT_ID")
	clientSecret := mustEnv("CF_CLIENT_SECRET")
	port := getEnv("PORT", "8080")

	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: getEnv("SKIP_TLS_VERIFY", "true") == "true"},
		},
	}

	tokens = newTokenManager(cfAPI, clientID, clientSecret)
	if err := tokens.Refresh(); err != nil {
		log.Fatalf("Failed to obtain initial token: %v", err)
	}
	go tokens.backgroundRefresh()

	// Parse endpoints from ENDPOINTS env var (JSON array)
	endpointsJSON := mustEnv("ENDPOINTS")
	if err := json.Unmarshal([]byte(endpointsJSON), &endpoints); err != nil {
		log.Fatalf("Failed to parse ENDPOINTS: %v", err)
	}
	log.Printf("Managing %d endpoints:", len(endpoints))
	for _, ep := range endpoints {
		log.Printf("  - %s (guid=%s, url=%s)", ep.Name, ep.GUID, ep.BackendURL)
	}

	hub = newWSHub()

	// Start billing event tracker
	billing = newBillingTracker(endpoints)
	go billing.backgroundPoll()

	// Initial state fetch
	refreshEndpointStates()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/events", sseHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/api/invoke", apiInvokeHandler)
	mux.HandleFunc("/api/stop", apiStopHandler)

	log.Printf("Scale-to-Zero Demo Coordinator on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func mustEnv(key string) string {
	val := os.Getenv(key)
	if val == "" {
		log.Fatalf("Required env var %s not set", key)
	}
	return val
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

// Ignore unused import warning for url package
var _ = url.QueryEscape
