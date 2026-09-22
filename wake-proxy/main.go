// Wake-proxy: CF app that intercepts requests for sleeping apps.
//
// Flow:
//  1. Receives request (gorouter sends it here because primary route points to wake-proxy)
//  2. Determines target app from Host header → config map
//  3. Creates or joins a wakeSession for this hostname (one poller per route, shared by all waiters)
//  4. Calls broker POST /wake/{app_guid} to start the app (leader goroutine only)
//  5. Polls target app via its SECONDARY route (always mapped to the real app)
//  6. Forwards held request to the real app via the secondary route
//  7. PATCH /v3/routes/{guid}/destinations to swap primary route back to real app (once per wake cycle)
//  8. Wake-proxy is out of the path — subsequent requests go directly to the app
//
// HA design: stateless per-instance. Each wake-proxy instance maintains independent wakeSessions.
// This works because CF API operations used here are idempotent:
//   - POST /v3/apps/.../start returns 422 if already running (harmless)
//   - Health polling the secondary route is read-only
//   - PATCH /v3/routes/.../destinations replaces all destinations with the same payload (idempotent)
// Multiple instances polling the same secondary route simultaneously = harmless redundancy.
// No inter-instance coordination or shared state is needed.
//
// Clean death (crash): no persistent mess to clean up.
//   - App started but route not swapped: next request to any surviving instance creates a new
//     wakeSession → health passes immediately (app already running) → forward works → swap fires.
//   - All instances die: primary route points to dead app → gorouter returns 502. CF process
//     health check restarts instances within ~30s, then normal flow resumes.
//   - No distributed lock, external state, or session handoff required.
//
// NOTE: getCCToken and the http.Client pattern are duplicated in broker/main.go.
// Two separately-deployed binaries with no shared packages — intentional for a PoC.
// Production: extract to an internal module.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// AppConfig maps a route hostname to its target app details.
type AppConfig struct {
	AppGUID     string `json:"app_guid"`
	AppRoute    string `json:"app_route"`    // secondary route — always mapped to real app (for polling + forwarding)
	RouteGUID   string `json:"route_guid"`   // primary route GUID (for PATCH /v3/routes/.../destinations)
	SpaceGUID   string `json:"space_guid"`
	AppPort     int    `json:"app_port"`     // usually 8080
	WakeTimeout int    `json:"wake_timeout"` // seconds to wait for healthy before 504; default 60
	HealthPath  string `json:"health_path"`  // health poll path from CC config; overridden by broker response; default "/"
}

func (c AppConfig) wakeTimeoutDuration() time.Duration {
	if c.WakeTimeout <= 0 {
		return 60 * time.Second
	}
	return time.Duration(c.WakeTimeout) * time.Second
}

// wakeSession is created on the first cold request for a hostname and shared by all
// concurrent requests until the app is healthy and the route swap has fired.
//
// Invariants:
//   - exactly ONE poller goroutine runs per session (started by the leader handler)
//   - exactly ONE route swap fires per wake cycle (session.once)
//   - done is closed exactly once (session.finOnce)
type wakeSession struct {
	cfg        AppConfig
	done       chan struct{}  // closed when app is ready OR failed (or timed out)
	err        error         // set before closing done; nil means healthy
	waiters    atomic.Int32  // count of requests currently waiting in this session (for 429 cap)
	once       sync.Once     // route swap: fires exactly once per wake cycle after first forward
	finOnce    sync.Once     // ensures done is closed exactly once (poller vs. timeout race)
	healthPath string        // resolved health poll path (from broker response, config, or "/")
}

// finish closes the session's done channel exactly once and records the outcome.
// Safe to call from concurrent goroutines (poller and timeout race to call it).
func (s *wakeSession) finish(err error) {
	s.finOnce.Do(func() {
		s.err = err
		close(s.done)
	})
}

const maxWaiters = 50 // hard safety cap per hostname — not configurable; it's a safety valve, not a knob

// maxBodyBytes limits buffered request bodies. With 50 concurrent waiters, worst case memory
// is 50 × 1MB = 50MB — fits in 64MB container with overhead. Beyond this, clients get 413.
const maxBodyBytes = 1 << 20 // 1 MB

var (
	config   map[string]AppConfig // hostname → AppConfig, loaded from WAKE_CONFIG env at startup
	configMu sync.RWMutex

	// sessions maps hostname → *wakeSession. sync.Map for lock-free concurrent handler access.
	// A session lives from first cold request until all waiters drain + 5s grace (see expireSession).
	sessions sync.Map

	brokerURL   string
	brokerToken string
	cfAPI       string
	cfUser     string
	cfPass     string
	port       string

	ccToken    string
	ccTokenExp time.Time
	ccTokenMu  sync.Mutex

	// draining is set on SIGTERM. New wake initiations are refused when true.
	// Requests that already joined an existing wakeSession continue to completion —
	// they're committed and blocking them would just leave an in-progress wake without a receiver.
	draining atomic.Bool

	// Single shared client for polling/broker calls — connection pooling across all requests.
	// InsecureSkipVerify: LOD landscapes use self-signed certs.
	client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Separate client for forwarding: no redirect following.
	// Why: redirects from the backend app must be passed through to the original
	// client unchanged (Location header intact). The default Go client follows them silently.
	fwdClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
)

func main() {
	brokerURL = requireEnv("BROKER_URL")
	brokerToken = requireEnv("BROKER_TOKEN")
	cfAPI = requireEnv("CF_API")
	cfUser = requireEnv("CF_ADMIN_USER")
	cfPass = requireEnv("CF_ADMIN_PASS")
	port = envOr("PORT", "8080")

	loadConfig()

	mux := http.NewServeMux()
	mux.HandleFunc("/_admin/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", proxyHandler)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Diego sends SIGTERM with a 10s grace window before SIGKILL.
	// On SIGTERM:
	//   1. Stop accepting NEW wake requests (return 503 + Retry-After: 1)
	//   2. Let in-flight requests drain (they're already waiting on a wakeSession)
	//   3. If drain doesn't complete in 8s, force-close (leave 2s for Diego cleanup)
	// Requests that arrive during drain and find an existing wakeSession still join it.
	// New wake initiations are refused.
	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
		<-quit

		log.Println("SIGTERM received — draining in-flight requests")
		draining.Store(true)

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown incomplete: %v", err)
		}
	}()

	log.Printf("s2z-wake-proxy on :%s (broker: %s)", port, brokerURL)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	log.Println("shutdown complete")
}

func loadConfig() {
	configJSON := os.Getenv("WAKE_CONFIG")
	if configJSON == "" {
		config = make(map[string]AppConfig)
		log.Println("WARN: WAKE_CONFIG not set, no apps configured")
		return
	}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		log.Fatalf("Failed to parse WAKE_CONFIG: %v", err)
	}
	log.Printf("Loaded config for %d apps", len(config))
	for host, cfg := range config {
		log.Printf("  %s → app=%s route=%s timeout=%ds", host, cfg.AppGUID, cfg.RouteGUID, cfg.WakeTimeout)
	}
}

// --- Main proxy handler ---

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	host := strings.Split(r.Host, ":")[0]

	configMu.RLock()
	appCfg, found := config[host]
	configMu.RUnlock()

	if !found {
		log.Printf("No config for host=%s", host)
		http.Error(w, fmt.Sprintf("no wake config for host %s", host), http.StatusNotFound)
		return
	}

	// Buffer request body BEFORE entering the wake flow. r.Body is a stream — consumed on
	// first read. Without buffering, POST/PUT retries in forwardRequest would send an empty body.
	// MaxBytesReader caps at 1MB to prevent OOM from large payloads × 50 concurrent waiters.
	var bodyBytes []byte
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
	}

	// Refuse new wake initiations while draining, but allow joining an in-progress session.
	// Checking before getOrCreateSession means there's a narrow race (SIGTERM between Load
	// and LoadOrStore), but the 8s drain timeout bounds the worst case.
	_, sessionExists := sessions.Load(host)
	if !sessionExists && draining.Load() {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "instance draining, retry on another instance immediately", http.StatusServiceUnavailable)
		return
	}

	session, isNew := getOrCreateSession(host, appCfg)

	// Hard cap on concurrent waiters per route. Atomic CAS loop eliminates the TOCTOU
	// race between Load and Add — exactly maxWaiters goroutines can join.
	for {
		cur := session.waiters.Load()
		if cur >= int32(maxWaiters) {
			log.Printf("WARN: waiter cap (%d) hit for host=%s", maxWaiters, host)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many concurrent wake requests, retry in 5s", http.StatusTooManyRequests)
			return
		}
		if session.waiters.CompareAndSwap(cur, cur+1) {
			break
		}
	}
	defer session.waiters.Add(-1)

	// Leader: start the single poller goroutine for this wake cycle.
	if isNew {
		go runWakeSession(host, session)
	}

	// All waiters (including leader) block here until the session resolves.
	<-session.done

	if session.err != nil {
		log.Printf("ERROR waking app %s: %v", appCfg.AppGUID, session.err)
		http.Error(w, "failed to wake app: "+session.err.Error(), http.StatusBadGateway)
		return
	}

	log.Printf("WAKE-PROXY: host=%s → app=%s healthy, forwarding", host, appCfg.AppGUID)
	forwardRequest(w, r, appCfg, bodyBytes)

	// Swap the primary route back to the real app exactly once per wake cycle.
	// All concurrent forwarders race to this once; only the first fires.
	// expireSession is started here so it begins counting down after the swap is scheduled.
	session.once.Do(func() {
		go func() {
			swapRoute(appCfg)
			expireSession(host, session)
		}()
	})
}

// getOrCreateSession returns the existing wakeSession for host, or creates a new one.
// Returns (session, isNew) — isNew=true means this call created it (caller is the leader).
// sync.Map.LoadOrStore handles the race between two goroutines both finding no session.
func getOrCreateSession(host string, cfg AppConfig) (*wakeSession, bool) {
	if v, ok := sessions.Load(host); ok {
		return v.(*wakeSession), false
	}

	healthPath := cfg.HealthPath
	if healthPath == "" {
		healthPath = "/"
	} else if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}
	s := &wakeSession{
		cfg:        cfg,
		done:       make(chan struct{}),
		healthPath: healthPath,
	}
	actual, loaded := sessions.LoadOrStore(host, s)
	if loaded {
		return actual.(*wakeSession), false
	}
	return s, true
}

// expireSession waits for all waiters to drain, then removes the session entry after
// a grace period. The 5s grace ensures any request that passed the waiters cap check
// but hasn't yet called waiters.Add(1) doesn't find a missing session and start a
// redundant wake cycle.
func expireSession(host string, s *wakeSession) {
	for s.waiters.Load() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(5 * time.Second)
	sessions.CompareAndDelete(host, s)
	log.Printf("session expired for host=%s", host)
}

// runWakeSession is the leader goroutine: one per wakeSession, started by the first
// request handler. Calls broker, then polls health until ready or timed out.
func runWakeSession(host string, s *wakeSession) {
	timeout := s.cfg.wakeTimeoutDuration()

	// Timeout goroutine races against the poller to call finish().
	// Using a timer (not time.After) so it can be stopped on normal completion
	// and not leak until it fires.
	timer := time.NewTimer(timeout)
	go func() {
		select {
		case <-timer.C:
			log.Printf("TIMEOUT waking host=%s after %v", host, timeout)
			s.finish(fmt.Errorf("timeout after %v waiting for app to become healthy", timeout))
		case <-s.done:
			timer.Stop()
		}
	}()

	// Call broker to start the app. Parses health check info from the response.
	healthPath, err := callBroker(s.cfg)
	if err != nil {
		s.finish(err)
		return
	}
	// Broker response takes precedence over config-level health path.
	if healthPath != "" {
		s.healthPath = healthPath
	}

	// Poll the secondary route every 300ms.
	// 300ms interval: faster than Diego scheduling (~1.5s) so we catch readiness within
	// one poll cycle; slow enough not to hammer gorouter during a thundering herd.
	healthURL := fmt.Sprintf("https://%s%s", s.cfg.AppRoute, s.healthPath)
	for {
		// Check if timeout already fired before each poll attempt.
		select {
		case <-s.done:
			return
		default:
		}

		req, _ := http.NewRequest("GET", healthURL, nil)
		resp, pollErr := client.Do(req)
		if pollErr == nil {
			resp.Body.Close()
			// Only 2xx-3xx counts as healthy.
			// 404 from gorouter = route not yet registered via route-emitter.
			// 502/503 = container starting but port not yet bound.
			// Both are transient during cold start.
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				log.Printf("app %s healthy via secondary route (status=%d)", s.cfg.AppGUID, resp.StatusCode)
				s.finish(nil)
				return
			}
		}

		time.Sleep(300 * time.Millisecond)
	}
}

// brokerWakeResponse is the structured response from POST /wake.
type brokerWakeResponse struct {
	Status  string `json:"status"`
	AppGUID string `json:"app_guid"`
	HealthCheck struct {
		Type              string `json:"type"`
		Endpoint          string `json:"endpoint"`
		InvocationTimeout int    `json:"invocation_timeout"`
	} `json:"health_check"`
}

// callBroker calls POST /wake on the broker to trigger app start.
// Returns the health check endpoint path to use for polling (may be empty if
// the health check type is not "http" — in which case the caller falls back to config or "/").
func callBroker(cfg AppConfig) (string, error) {
	wakeURL := fmt.Sprintf("%s/wake/%s", brokerURL, cfg.AppGUID)
	req, _ := http.NewRequest("POST", wakeURL, nil)
	req.Header.Set("Authorization", "Bearer "+brokerToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker call: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("broker returned %d: %s", resp.StatusCode, string(body))
	}

	var brokerResp brokerWakeResponse
	if err := json.Unmarshal(body, &brokerResp); err != nil {
		// Older broker versions may return a bare 200 without a structured body.
		log.Printf("WARN: could not parse broker response body: %v", err)
		return "", nil
	}

	// Only http-type health checks have a meaningful endpoint path to poll.
	// For "process" or "port" types there's no HTTP endpoint — fall back to "/".
	if brokerResp.HealthCheck.Type == "http" && brokerResp.HealthCheck.Endpoint != "" {
		return brokerResp.HealthCheck.Endpoint, nil
	}
	return "", nil
}

// --- Forward request via secondary route with retry ---
// After cold wake, multiple gorouters behind HAProxy may have inconsistent route
// tables for up to ~2s (NATS fan-out is not atomic). Retry with exponential backoff
// (200ms → 3.2s) handles 502/503/404 from unlucky LB picks during convergence.
//
// Why manual http.Client instead of httputil.ReverseProxy:
// ReverseProxy commits the HTTP status to the client on first WriteHeader — it cannot
// retry on 502/503 because the response is already in flight.

func forwardRequest(w http.ResponseWriter, r *http.Request, cfg AppConfig, bodyBytes []byte) {
	target, _ := url.Parse(fmt.Sprintf("https://%s", cfg.AppRoute))

	var resp *http.Response
	var lastErr error
	const maxRetries = 5
	backoff := 200 * time.Millisecond // 200ms → 400 → 800 → 1600 → 3200ms

	for attempt := 0; attempt < maxRetries; attempt++ {
		fwdURL := *target
		fwdURL.Path = r.URL.Path
		fwdURL.RawQuery = r.URL.RawQuery

		// Fresh reader from buffered body on each attempt.
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		req, _ := http.NewRequest(r.Method, fwdURL.String(), bodyReader)
		for k, vv := range r.Header {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		req.Host = target.Host
		if bodyBytes != nil {
			req.ContentLength = int64(len(bodyBytes))
		}

		resp, lastErr = fwdClient.Do(req)
		if lastErr != nil {
			log.Printf("forward attempt %d transport error: %v", attempt+1, lastErr)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		if resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 404 {
			resp.Body.Close()
			log.Printf("forward attempt %d got %d, retrying in %v", attempt+1, resp.StatusCode, backoff)
			time.Sleep(backoff)
			backoff *= 2
			continue
		}
		break
	}

	if lastErr != nil {
		http.Error(w, fmt.Sprintf("forward failed after %d attempts: %v", maxRetries, lastErr), http.StatusBadGateway)
		return
	}
	if resp == nil {
		http.Error(w, "no response from backend", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// --- Route swap: PATCH /v3/routes/{guid}/destinations ---
// Replaces ALL destinations on the primary route in one atomic call.
// After this propagates (~1s via NATS), gorouter sends requests directly to the
// app and wake-proxy receives no more traffic for this hostname.

func swapRoute(cfg AppConfig) {
	if cfg.RouteGUID == "" {
		log.Printf("No route_guid for app %s, skipping swap", cfg.AppGUID)
		return
	}

	token, err := getCCToken()
	if err != nil {
		log.Printf("ERROR getting CC token for route swap: %v", err)
		return
	}

	payload := map[string]interface{}{
		"destinations": []map[string]interface{}{
			{
				"app":      map[string]interface{}{"guid": cfg.AppGUID},
				"port":     cfg.AppPort,
				"protocol": "http1",
			},
		},
	}

	body, _ := json.Marshal(payload)
	destURL := fmt.Sprintf("%s/v3/routes/%s/destinations", cfAPI, cfg.RouteGUID)
	req, _ := http.NewRequest("PATCH", destURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ERROR route swap PATCH: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("ROUTE SWAP success: route=%s → app=%s", cfg.RouteGUID, cfg.AppGUID)
	} else {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("ROUTE SWAP failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}
}

// --- CC token (duplicated from broker — see package header note) ---

// getCCToken returns a cached CC OAuth2 token, refreshing via password grant
// when expired. Discovers UAA endpoint from /v2/info on each refresh.
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
	formPayload := url.Values{
		"grant_type": {"password"},
		"username":   {cfUser},
		"password":   {cfPass},
	}.Encode()
	req, _ := http.NewRequest("POST", tokenURL, strings.NewReader(formPayload))
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

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var not set: %s", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
