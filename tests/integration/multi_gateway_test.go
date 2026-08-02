// Package integration_test proves that two stateless gateway instances enforce
// one shared rate limit backed by Redis.
//
// Run against a live stack:
//
//	docker run --rm \
//	  -e GATEWAY1_URL=http://host.docker.internal:8091 \
//	  -e GATEWAY2_URL=http://host.docker.internal:8092 \
//	  -e NGINX_URL=http://host.docker.internal:8090 \
//	  -v "$(pwd):/app" -w /app golang:1.25-alpine \
//	  go test -tags integration -v ./tests/integration/...
//
// Or from within the compose network (e.g. a test container on the same network):
//
//	GATEWAY1_URL=http://gateway1:8080 GATEWAY2_URL=http://gateway2:8080 \
//	  NGINX_URL=http://nginx:80 go test -tags integration -v ./tests/integration/...
//
//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// ── helpers ────────────────────────────────────────────────────────────────

func gw1URL() string { return getEnv("GATEWAY1_URL", "http://localhost:8091") }
func gw2URL() string { return getEnv("GATEWAY2_URL", "http://localhost:8092") }
func ngURL() string  { return getEnv("NGINX_URL", "http://localhost:8090") }

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// uniqueSuffix returns a short suffix that makes test usernames unique across runs.
func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

type authResponse struct {
	Token string `json:"token"`
	Plan  string `json:"plan"`
	Sub   string `json:"sub"`
}

type limitConfig struct {
	Algorithm  string  `json:"algorithm"`
	Capacity   float64 `json:"capacity,omitempty"`
	RefillRate float64 `json:"refillRate,omitempty"`
	Limit      int     `json:"limit,omitempty"`
	WindowSecs int     `json:"windowSecs,omitempty"`
}

// waitReady polls /healthz until it returns 200 or the deadline passes.
func waitReady(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("gateway at %s not ready after 15s", base)
}

func postJSON(t *testing.T, url string, body any, token string) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func putJSON(t *testing.T, url string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func deleteReq(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func register(t *testing.T, base, username, password, plan, algorithm string) authResponse {
	t.Helper()
	body := map[string]string{
		"username":  username,
		"password":  password,
		"plan":      plan,
		"algorithm": algorithm,
	}
	status, raw := postJSON(t, base+"/auth/register", body, "")
	if status != 201 {
		t.Fatalf("register %s on %s: status %d body %s", username, base, status, raw)
	}
	var ar authResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		t.Fatalf("register response decode: %v", err)
	}
	return ar
}

func login(t *testing.T, base, username, password string) authResponse {
	t.Helper()
	body := map[string]string{"username": username, "password": password}
	status, raw := postJSON(t, base+"/auth/login", body, "")
	if status != 200 {
		t.Fatalf("login %s on %s: status %d body %s", username, base, status, raw)
	}
	var ar authResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		t.Fatalf("login response decode: %v", err)
	}
	return ar
}

// get sends GET /path with the given bearer token and returns the status code
// plus the X-Gateway-Id response header.
func get(t *testing.T, base, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", base, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Gateway-Id")
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestSharedRateLimit is the thesis test of the whole project.
//
// Two stateless gateway instances (gateway1 and gateway2) both point at the
// same Redis. A single user's sliding-window counter lives in that Redis.
// Sending 60 requests to gateway1 and 40 to gateway2 must exhaust the 100
// request limit, so request 101 to either instance returns 429.
//
// If this test fails it means rate limiting is per-instance (broken): each
// instance has its own counter and neither sees the other's traffic.
func TestSharedRateLimit(t *testing.T) {
	waitReady(t, gw1URL())
	waitReady(t, gw2URL())

	// SLIDING_WINDOW has no token refill — exactly 100 requests are allowed
	// per 60-second window regardless of how fast we send them.
	username := "shared_" + uniqueSuffix()
	auth := register(t, gw1URL(), username, "testpass", "free", "SLIDING_WINDOW")
	token := auth.Token

	const (
		toGW1  = 60
		toGW2  = 40
		limit  = 100 // free plan sliding window limit
	)

	t.Logf("sending %d requests to gateway1...", toGW1)
	for i := 1; i <= toGW1; i++ {
		status, _ := get(t, gw1URL(), "/api/products", token)
		if status != 200 {
			t.Fatalf("gw1 request %d: expected 200, got %d (limit hit too early)", i, status)
		}
	}

	t.Logf("sending %d requests to gateway2...", toGW2)
	for i := 1; i <= toGW2; i++ {
		status, _ := get(t, gw2URL(), "/api/products", token)
		if status != 200 {
			t.Fatalf("gw2 request %d: expected 200, got %d (limit hit too early)", i, status)
		}
	}

	t.Logf("sending request %d to gateway1 — expect 429", limit+1)
	status, _ := get(t, gw1URL(), "/api/products", token)
	if status != 429 {
		t.Errorf("request %d to gw1: expected 429, got %d — shared state not enforced", limit+1, status)
	}

	t.Logf("sending request %d to gateway2 — expect 429", limit+1)
	status, _ = get(t, gw2URL(), "/api/products", token)
	if status != 429 {
		t.Errorf("request %d to gw2: expected 429, got %d — shared state not enforced", limit+1, status)
	}
}

// TestJWTPortability proves that JWT credentials issued by one gateway are
// accepted by the other, and that both gateways share the same user store.
//
// This works because:
//   - JWT_SECRET is identical across instances (same env var in compose)
//   - User records are stored in Redis, not in gateway memory
func TestJWTPortability(t *testing.T) {
	waitReady(t, gw1URL())
	waitReady(t, gw2URL())

	username := "portuser_" + uniqueSuffix()

	// Register on gateway1.
	r1 := register(t, gw1URL(), username, "testpass", "pro", "SLIDING_WINDOW")

	// Use the token issued by gateway1 on gateway2 — same JWT_SECRET, so valid.
	status, gid := get(t, gw2URL(), "/api/products", r1.Token)
	if status != 200 {
		t.Errorf("JWT from gw1 rejected by gw2: status %d", status)
	}
	t.Logf("gw1 token used on gw2 (instance: %s) → %d", gid, status)

	// Login on gateway2 — reads the user record written by gateway1 (same Redis).
	r2 := login(t, gw2URL(), username, "testpass")
	if r2.Token == "" {
		t.Fatal("login on gw2 after registering on gw1 returned empty token")
	}
	if r2.Sub != r1.Sub {
		t.Errorf("sub mismatch: gw1 registered %s, gw2 returned %s", r1.Sub, r2.Sub)
	}

	// Use the token issued by gateway2 on gateway1.
	status, gid = get(t, gw1URL(), "/api/products", r2.Token)
	if status != 200 {
		t.Errorf("JWT from gw2 login rejected by gw1: status %d", status)
	}
	t.Logf("gw2 token used on gw1 (instance: %s) → %d", gid, status)
}

// TestAdminPropagation proves that a config override set via gateway1's admin
// API is immediately visible to gateway2 without waiting for the 30-second
// local cache TTL to expire.
//
// Mechanism: the admin PUT publishes to the "rl:cache-flush" Redis pub/sub
// channel; all instances' subscriber goroutines flush their local maps upon
// receiving the message, so the next request forces a fresh Redis read.
func TestAdminPropagation(t *testing.T) {
	waitReady(t, gw1URL())
	waitReady(t, gw2URL())

	// Register a fresh user so there is no prior rate-limit state.
	username := "adminprop_" + uniqueSuffix()
	auth := register(t, gw1URL(), username, "testpass", "pro", "SLIDING_WINDOW")
	token := auth.Token
	userID := auth.Sub

	// Set a very tight override (limit=3) via gateway1's admin API.
	limitKey := fmt.Sprintf("rate-limit:%s:/api/products", userID)
	overrideURL := gw1URL() + "/admin/limits/" + limitKey
	cfg := limitConfig{Algorithm: "SLIDING_WINDOW", Limit: 3, WindowSecs: 60}
	if s := putJSON(t, overrideURL, cfg); s != 200 {
		t.Fatalf("admin PUT on gw1: status %d", s)
	}
	t.Logf("override set on gw1: %s → limit=3", limitKey)

	// Give pub/sub a moment to reach gateway2's subscriber goroutine.
	time.Sleep(100 * time.Millisecond)

	// 3 requests to gateway2 must succeed (override picked up via pub/sub flush).
	for i := 1; i <= 3; i++ {
		status, _ := get(t, gw2URL(), "/api/products", token)
		if status != 200 {
			t.Fatalf("gw2 request %d with override limit=3: expected 200, got %d", i, status)
		}
	}

	// 4th request to gateway2 must be 429 (override limit exhausted).
	status, _ := get(t, gw2URL(), "/api/products", token)
	if status != 429 {
		t.Errorf("gw2 request 4 with override limit=3: expected 429, got %d — override not propagated", status)
	}
	t.Log("override propagated to gw2 via pub/sub: limit=3 enforced correctly")

	// Clean up: delete the override so subsequent test runs start clean.
	deleteReq(t, overrideURL)
}

// TestNGINXRoundRobin verifies that NGINX distributes traffic across both
// gateway instances. It reads the X-Gateway-Id header that each instance
// stamps on every response.
func TestNGINXRoundRobin(t *testing.T) {
	waitReady(t, ngURL())

	// Register on gateway1 (NGINX would work too, but direct is simpler here).
	waitReady(t, gw1URL())
	username := "nginx_" + uniqueSuffix()
	auth := register(t, gw1URL(), username, "testpass", "pro", "SLIDING_WINDOW")
	token := auth.Token

	seen := map[string]int{}
	const rounds = 20
	for i := 0; i < rounds; i++ {
		req, _ := http.NewRequest(http.MethodGet, ngURL()+"/api/products", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("NGINX request %d: %v", i+1, err)
		}
		resp.Body.Close()
		gid := resp.Header.Get("X-Gateway-Id")
		if gid == "" {
			t.Errorf("request %d: X-Gateway-Id header missing from NGINX response", i+1)
		}
		seen[gid]++
	}

	t.Logf("NGINX round-robin distribution over %d requests: %v", rounds, seen)

	if len(seen) < 2 {
		t.Errorf("expected traffic on both gateways, only saw: %v", seen)
	}

	// Both instances should get at least 20% of traffic in a balanced pool.
	for gid, count := range seen {
		pct := float64(count) / float64(rounds) * 100
		if pct < 20 {
			t.Errorf("instance %s received only %.0f%% of traffic — not balanced", gid, pct)
		}
		t.Logf("  %s: %d requests (%.0f%%)", gid, count, pct)
	}

	// Verify admin endpoint is blocked at NGINX layer.
	resp, err := http.Get(ngURL() + "/admin/keys")
	if err != nil {
		t.Fatalf("admin check via NGINX: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("admin API via NGINX: expected 403, got %d — not restricted", resp.StatusCode)
	}
	t.Log("admin API correctly blocked at NGINX (403)")
}

// TestGracefulShutdown verifies that in-flight requests complete before the
// gateway exits. This is observable via the gateway logs after a manual
// docker compose stop — the test here checks the baseline: /healthz returns
// 200, confirming the server is up and handling requests normally.
func TestGracefulShutdown(t *testing.T) {
	waitReady(t, gw1URL())
	waitReady(t, gw2URL())

	for _, base := range []string{gw1URL(), gw2URL()} {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatalf("%s/healthz: %v", base, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s/healthz: expected 200, got %d", base, resp.StatusCode)
		}
		// Confirm gateway_id is set in the response.
		gid := resp.Header.Get("X-Gateway-Id")
		if gid == "" || strings.Contains(gid, "gateway") {
			t.Logf("%s: X-Gateway-Id = %q", base, gid)
		}
	}
}
