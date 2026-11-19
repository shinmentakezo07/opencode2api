package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/models"
)

func TestBreakerStateTransitions(t *testing.T) {
	pool := &anonymousNodes{backoff: anonymousBackoff{threshold: 2, base: time.Minute, max: 8 * time.Minute}}
	now := time.Now()

	if got := pool.Snapshot(now).State; got != breakerClosed {
		t.Fatalf("initial state = %q, want closed", got)
	}
	if !pool.AllowAttempt() {
		t.Fatal("closed breaker refused an attempt")
	}

	pool.NoteFailure()
	if got := pool.Snapshot(now).State; got != breakerClosed {
		t.Fatalf("state after one failure = %q, want closed (threshold 2)", got)
	}

	pause, tripped := pool.NoteFailure()
	if !tripped || pause <= 0 {
		t.Fatalf("second failure did not trip the breaker: pause=%v tripped=%v", pause, tripped)
	}
	open := pool.Snapshot(now)
	if open.State != breakerOpen {
		t.Fatalf("state after trip = %q, want open", open.State)
	}
	if open.PauseUntil.IsZero() {
		t.Fatal("open breaker has no pause_until")
	}
	if pool.AllowAttempt() {
		t.Fatal("open breaker allowed an attempt")
	}

	// Expire the window to enter half-open and verify the single probe slot.
	pool.breakerUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if got := pool.Snapshot(time.Now()).State; got != breakerHalfOpen {
		t.Fatalf("state after window expiry = %q, want half-open", got)
	}
	if !pool.AllowAttempt() {
		t.Fatal("half-open breaker refused the probe")
	}
	if pool.AllowAttempt() {
		t.Fatal("half-open breaker allowed a second probe")
	}
	pool.ReleaseProbe()
	if !pool.AllowAttempt() {
		t.Fatal("released probe was not reusable")
	}

	// A failure during the probe reopens immediately with a longer window.
	pool.NoteFailure()
	if got := pool.Snapshot(time.Now()).State; got != breakerOpen {
		t.Fatalf("state after failed probe = %q, want open", got)
	}

	pool.NoteSuccess()
	closed := pool.Snapshot(time.Now())
	if closed.State != breakerClosed || closed.Strikes != 0 || closed.ConsecutiveFailures != 0 {
		t.Fatalf("state after success = %#v, want closed with no strikes", closed)
	}
}

func TestBreakerDisabledAlwaysClosed(t *testing.T) {
	pool := &anonymousNodes{}
	if got := pool.Snapshot(time.Now()).State; got != breakerClosed {
		t.Fatalf("disabled breaker state = %q, want closed", got)
	}
	pool.ForceOpen()
	if got := pool.Snapshot(time.Now()).State; got != breakerClosed {
		t.Fatalf("disabled breaker opened: %q", got)
	}
}

func TestProxyState(t *testing.T) {
	now := time.Now()
	proxy := &proxyTransport{index: 0, name: "direct"}
	if got := proxyState(proxy, now); got != ProxyStateUnhealthy {
		t.Fatalf("initial state = %q, want unhealthy", got)
	}
	proxy.healthy.Store(true)
	if got := proxyState(proxy, now); got != ProxyStateHealthy {
		t.Fatalf("healthy state = %q", got)
	}
	proxy.checking.Store(true)
	if got := proxyState(proxy, now); got != ProxyStateChecking {
		t.Fatalf("checking state = %q", got)
	}
	proxy.markBlocked(now.Add(time.Minute))
	if got := proxyState(proxy, now); got != ProxyStateBlocked {
		t.Fatalf("blocked state = %q", got)
	}
	if got := proxyState(proxy, now.Add(2*time.Minute)); got != ProxyStateChecking {
		t.Fatalf("state after block expiry = %q, want checking", got)
	}
}

func TestKeyStatusState(t *testing.T) {
	pool := &upstreamNodes{nodes: []*upstreamNode{{key: "sk-test", index: 0}}}
	statuses := keyStatuses("zen", pool)
	if len(statuses) != 1 || statuses[0].State != KeyStateReady || statuses[0].CooldownUntil != nil {
		t.Fatalf("ready status = %#v", statuses)
	}
	pool.nodes[0].cooldownUntil.Store(time.Now().Add(time.Minute).UnixNano())
	statuses = keyStatuses("zen", pool)
	if statuses[0].State != KeyStateCooling || statuses[0].CooldownUntil == nil {
		t.Fatalf("cooling status = %#v", statuses)
	}
}

func TestAvailabilityViewJSON(t *testing.T) {
	checked := time.Now().UTC()
	view := AvailabilityView{
		Availability: models.Availability{Model: "free-model", CheckedAt: checked},
		Status:       models.AvailabilityAvailable,
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"model":"free-model"`, `"status":"available"`, `"checked_at"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("json missing %s: %s", want, text)
		}
	}
}
