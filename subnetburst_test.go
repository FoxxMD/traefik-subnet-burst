package traefik_subnet_burst

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, mutate func(*Config)) (*SubnetBurst, *clock) {
	t.Helper()
	cfg := CreateConfig()
	cfg.CountPaths = []string{"/api/script.js"}
	if mutate != nil {
		mutate(cfg)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h, err := New(context.Background(), ok, cfg, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	sb := h.(*SubnetBurst)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	sb.now = c.now
	return sb, c
}

func do(sb *SubnetBurst, remote, path string, headers ...string) int {
	req := httptest.NewRequest(http.MethodGet, "https://ry.example.com"+path, nil)
	req.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	sb.ServeHTTP(rec, req)
	return rec.Code
}

func expect(t *testing.T, got, want int, msg string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %d, want %d", msg, got, want)
	}
}

// Replays the burst from the issue: distinct IPs in 195.64.0.0/16 load script.js.
func TestDistinctIPsBlockSubnet(t *testing.T) {
	sb, c := setup(t, nil)

	expect(t, do(sb, "195.64.119.47:37367", "/api/script.js"), 200, "1st IP")
	expect(t, do(sb, "195.64.119.47:42575", "/api/track"), 200, "uncounted path")
	expect(t, do(sb, "195.64.115.161:58677", "/api/script.js"), 200, "2nd IP")
	expect(t, do(sb, "5.183.91.175:24163", "/api/script.js"), 200, "other subnet")
	c.advance(2 * time.Second)
	expect(t, do(sb, "195.64.3.3:1", "/api/script.js"), 403, "3rd distinct IP triggers")

	// Whole subnet blocked on every path, other subnets untouched.
	expect(t, do(sb, "195.64.200.1:1", "/api/track"), 403, "subnet blocked on any path")
	expect(t, do(sb, "5.183.91.175:1", "/api/track"), 200, "other subnet still open")

	c.advance(time.Hour)
	expect(t, do(sb, "195.64.119.47:1", "/api/script.js"), 200, "block expired")
}

func TestRepeatsFromOneIPDoNotTriggerDistinct(t *testing.T) {
	sb, _ := setup(t, nil)
	for i := 0; i < 10; i++ {
		expect(t, do(sb, "195.64.119.47:1", "/api/script.js"), 200, "same IP repeat")
	}
}

func TestRequestCountModeAndWindow(t *testing.T) {
	sb, c := setup(t, func(cfg *Config) { cfg.DistinctIPs = false })
	expect(t, do(sb, "10.0.0.1:1", "/api/script.js"), 200, "1")
	expect(t, do(sb, "10.0.0.1:1", "/api/script.js"), 200, "2")
	c.advance(121 * time.Second) // both hits leave the 120s window
	expect(t, do(sb, "10.0.0.1:1", "/api/script.js"), 200, "window reset")
	expect(t, do(sb, "10.0.0.1:1", "/api/script.js"), 200, "2 in window")
	expect(t, do(sb, "10.0.0.1:1", "/api/script.js"), 403, "3rd in window")
}

func TestAllowedCIDRs(t *testing.T) {
	sb, _ := setup(t, func(cfg *Config) { cfg.Threshold = 1; cfg.AllowedCIDRs = []string{"203.0.113.0/24"} })
	expect(t, do(sb, "203.0.113.9:1", "/api/script.js"), 200, "allowed")
	expect(t, do(sb, "203.0.114.9:1", "/api/script.js"), 403, "not allowed")
}

func TestIPv6Grouping(t *testing.T) {
	sb, _ := setup(t, func(cfg *Config) { cfg.Threshold = 2 })
	expect(t, do(sb, "[2001:db8:1:1::1]:1", "/api/script.js"), 200, "1st")
	expect(t, do(sb, "[2001:db8:1:2::1]:1", "/api/script.js"), 403, "same /48")
	expect(t, do(sb, "[2001:db8:2::1]:1", "/api/track"), 200, "other /48")
}

func TestIPHeaderAndTrustedProxies(t *testing.T) {
	sb, _ := setup(t, func(cfg *Config) {
		cfg.Threshold = 1
		cfg.IPHeader = "X-Forwarded-For"
		cfg.TrustedProxies = []string{"172.16.0.0/12"}
	})
	// From a trusted proxy, the last header entry is the client.
	expect(t, do(sb, "172.18.0.2:1", "/api/track", "X-Forwarded-For", "6.6.6.6, 195.64.1.1"), 200, "uncounted path, no block yet")
	expect(t, do(sb, "172.18.0.2:1", "/api/script.js", "X-Forwarded-For", "6.6.6.6, 195.64.1.1"), 403, "header IP counted")
	expect(t, do(sb, "172.18.0.2:1", "/api/track", "X-Forwarded-For", "195.64.9.9"), 403, "header subnet blocked")
	// From an untrusted connection, the header is ignored.
	expect(t, do(sb, "8.8.8.8:1", "/api/track", "X-Forwarded-For", "195.64.9.9"), 200, "spoofed header ignored")
}

func TestStateSurvivesRebuild(t *testing.T) {
	sb, c := setup(t, func(cfg *Config) { cfg.Threshold = 1 })
	expect(t, do(sb, "195.64.1.1:1", "/api/script.js"), 403, "trigger")
	// Traefik calls New again with the same middleware name on config reload.
	h, err := New(context.Background(), sb.next, &sb.cfg, sb.name)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := h.(*SubnetBurst)
	rebuilt.now = c.now
	expect(t, do(rebuilt, "195.64.2.2:1", "/api/track"), 403, "still blocked after rebuild")
}

func TestSweepRemovesIdleBuckets(t *testing.T) {
	sb, c := setup(t, nil)
	do(sb, "195.64.1.1:1", "/api/script.js")
	do(sb, "5.183.1.1:1", "/api/script.js")
	c.advance(5 * time.Minute)
	do(sb, "9.9.9.9:1", "/api/script.js")
	if n := len(sb.store.buckets); n != 1 {
		t.Fatalf("got %d buckets after sweep, want 1", n)
	}
}

func TestInvalidConfig(t *testing.T) {
	cfg := CreateConfig()
	cfg.PrefixLengthV4 = 33
	if _, err := New(context.Background(), http.NotFoundHandler(), cfg, "bad"); err == nil {
		t.Fatal("expected error for prefixLengthV4=33")
	}
}
