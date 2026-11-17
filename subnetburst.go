// Package traefik_subnet_burst is a Traefik middleware plugin that groups clients
// into subnets and temporarily blocks a whole subnet after it sends a burst of
// requests within a time window.
package traefik_subnet_burst

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Config is the plugin configuration.
type Config struct {
	// PrefixLengthV4 is the subnet size IPv4 clients are grouped by.
	PrefixLengthV4 int `json:"prefixLengthV4,omitempty"`
	// PrefixLengthV6 is the subnet size IPv6 clients are grouped by.
	PrefixLengthV6 int `json:"prefixLengthV6,omitempty"`
	// CountPaths lists path prefixes that count toward a burst. Empty counts every request.
	// Blocking always applies to every request passing through the middleware.
	CountPaths []string `json:"countPaths,omitempty"`
	// Threshold is how many counted requests (or distinct IPs) inside the window trigger a block.
	Threshold int `json:"threshold,omitempty"`
	// WindowSeconds is the sliding window counted requests are kept for.
	WindowSeconds int `json:"windowSeconds,omitempty"`
	// DistinctIPs counts distinct client IPs in the subnet instead of raw requests.
	DistinctIPs bool `json:"distinctIPs,omitempty"`
	// BlockSeconds is how long a subnet stays blocked once triggered.
	BlockSeconds int `json:"blockSeconds,omitempty"`
	// StatusCode is the response status sent to blocked requests.
	StatusCode int `json:"statusCode,omitempty"`
	// AllowedCIDRs are never counted or blocked.
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`
	// IPHeader reads the client IP from this header (e.g. X-Real-Ip, CF-Connecting-IP)
	// instead of the connection address. Only use a header your proxy chain overwrites.
	IPHeader string `json:"ipHeader,omitempty"`
	// TrustedProxies limits IPHeader to requests whose connection address is in one of these CIDRs.
	// Empty trusts the header from any connection.
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// Debug logs every decision the middleware makes. Plugins don't inherit Traefik's
	// global log level, so this is set per middleware. Expect a line per request.
	Debug bool `json:"debug,omitempty"`
}

// CreateConfig returns the default configuration.
func CreateConfig() *Config {
	return &Config{
		PrefixLengthV4: 16,
		PrefixLengthV6: 48,
		Threshold:      3,
		WindowSeconds:  120,
		DistinctIPs:    true,
		BlockSeconds:   3600,
		StatusCode:     http.StatusForbidden,
	}
}

type hit struct {
	addr netip.Addr
	at   time.Time
}

type bucket struct {
	hits         []hit
	blockedUntil time.Time
}

// store holds bucket state. It lives in a package-level map keyed by middleware name
// so counters and active blocks survive Traefik rebuilding the middleware on config reloads.
type store struct {
	mu        sync.Mutex
	buckets   map[netip.Prefix]*bucket
	lastSweep time.Time
}

var (
	storesMu sync.Mutex
	stores   = map[string]*store{}
)

func getStore(name string) *store {
	storesMu.Lock()
	defer storesMu.Unlock()
	s, ok := stores[name]
	if !ok {
		s = &store{buckets: map[netip.Prefix]*bucket{}}
		stores[name] = s
	}
	return s
}

// SubnetBurst is the middleware.
type SubnetBurst struct {
	next    http.Handler
	name    string
	cfg     Config
	window  time.Duration
	block   time.Duration
	allowed []netip.Prefix
	trusted []netip.Prefix
	store   *store
	now     func() time.Time
	logger  *log.Logger
}

// New creates the middleware.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	cfg := *config
	if cfg.PrefixLengthV4 < 1 || cfg.PrefixLengthV4 > 32 {
		return nil, fmt.Errorf("prefixLengthV4 must be between 1 and 32, got %d", cfg.PrefixLengthV4)
	}
	if cfg.PrefixLengthV6 < 1 || cfg.PrefixLengthV6 > 128 {
		return nil, fmt.Errorf("prefixLengthV6 must be between 1 and 128, got %d", cfg.PrefixLengthV6)
	}
	if cfg.Threshold < 1 {
		return nil, fmt.Errorf("threshold must be at least 1, got %d", cfg.Threshold)
	}
	if cfg.WindowSeconds < 1 || cfg.BlockSeconds < 1 {
		return nil, fmt.Errorf("windowSeconds and blockSeconds must be at least 1")
	}
	if cfg.StatusCode < 100 || cfg.StatusCode > 599 {
		return nil, fmt.Errorf("statusCode must be a valid HTTP status, got %d", cfg.StatusCode)
	}
	allowed, err := parsePrefixes(cfg.AllowedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("allowedCIDRs: %w", err)
	}
	trusted, err := parsePrefixes(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trustedProxies: %w", err)
	}

	sb := &SubnetBurst{
		next:    next,
		name:    name,
		cfg:     cfg,
		window:  time.Duration(cfg.WindowSeconds) * time.Second,
		block:   time.Duration(cfg.BlockSeconds) * time.Second,
		allowed: allowed,
		trusted: trusted,
		store:   getStore(name),
		now:     time.Now,
		logger:  log.New(os.Stdout, "", 0),
	}
	sb.info("grouping by /%d (v4) /%d (v6), block after %d %s in %ds for %ds",
		cfg.PrefixLengthV4, cfg.PrefixLengthV6, cfg.Threshold, unit(cfg.DistinctIPs), cfg.WindowSeconds, cfg.BlockSeconds)
	sb.debug("countPaths=%v statusCode=%d allowedCIDRs=%v ipHeader=%q trustedProxies=%v",
		cfg.CountPaths, cfg.StatusCode, allowed, cfg.IPHeader, trusted)
	if cfg.Debug {
		sb.store.mu.Lock()
		n := len(sb.store.buckets)
		sb.store.mu.Unlock()
		sb.debug("tracking state for %d subnets carried over from previous instance", n)
	}
	return sb, nil
}

func (sb *SubnetBurst) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	addr, ok := sb.clientIP(req)
	if !ok {
		sb.debug("passing request: could not parse client IP from remoteAddr=%q path=%s", req.RemoteAddr, req.URL.Path)
		sb.next.ServeHTTP(rw, req)
		return
	}
	if contains(sb.allowed, addr) {
		sb.debug("passing request: ip=%s is in allowedCIDRs path=%s", addr, req.URL.Path)
		sb.next.ServeHTTP(rw, req)
		return
	}
	bits := sb.cfg.PrefixLengthV4
	if addr.Is6() {
		bits = sb.cfg.PrefixLengthV6
	}
	subnet, err := addr.Prefix(bits)
	if err != nil {
		sb.debug("passing request: could not compute /%d subnet for ip=%s: %v", bits, addr, err)
		sb.next.ServeHTTP(rw, req)
		return
	}

	if sb.record(subnet, addr, req.URL.Path) {
		rw.WriteHeader(sb.cfg.StatusCode)
		return
	}
	sb.next.ServeHTTP(rw, req)
}

// record updates the subnet's bucket and reports whether this request must be blocked.
// ponytail: one global lock per middleware; shard by subnet if request volume ever makes it contend.
func (sb *SubnetBurst) record(subnet netip.Prefix, addr netip.Addr, path string) bool {
	now := sb.now()
	s := sb.store
	s.mu.Lock()
	defer s.mu.Unlock()

	sb.sweep(now)

	b := s.buckets[subnet]
	if b != nil && !b.blockedUntil.IsZero() {
		if now.Before(b.blockedUntil) {
			sb.debug("blocking request: subnet=%s ip=%s path=%s status=%d remaining=%s",
				subnet, addr, path, sb.cfg.StatusCode, b.blockedUntil.Sub(now).Round(time.Second))
			return true
		}
		b.blockedUntil = time.Time{}
		sb.info("block expired for subnet=%s", subnet)
	}
	if !sb.counts(path) {
		sb.debug("passing request: path not counted subnet=%s ip=%s path=%s", subnet, addr, path)
		return false
	}
	if b == nil {
		b = &bucket{}
		s.buckets[subnet] = b
	}

	// Drop hits that left the window.
	cutoff := now.Add(-sb.window)
	kept := b.hits[:0]
	for _, h := range b.hits {
		if h.at.After(cutoff) && !(sb.cfg.DistinctIPs && h.addr == addr) {
			kept = append(kept, h)
		}
	}
	b.hits = append(kept, hit{addr: addr, at: now})

	if len(b.hits) < sb.cfg.Threshold {
		sb.debug("passing request: counted subnet=%s ip=%s path=%s %s=%d/%d window=%s",
			subnet, addr, path, unit(sb.cfg.DistinctIPs), len(b.hits), sb.cfg.Threshold, sb.window)
		return false
	}
	sb.info("blocking subnet=%s for %s: %d %s in %s (%s)",
		subnet, sb.block, len(b.hits), unit(sb.cfg.DistinctIPs), sb.window, hitIPs(b.hits))
	b.hits = nil
	b.blockedUntil = now.Add(sb.block)
	return true
}

// sweep removes idle buckets at most once per window. Caller holds s.mu.
func (sb *SubnetBurst) sweep(now time.Time) {
	s := sb.store
	if now.Sub(s.lastSweep) < sb.window {
		return
	}
	s.lastSweep = now
	cutoff := now.Add(-sb.window)
	removed := 0
	for k, b := range s.buckets {
		if now.Before(b.blockedUntil) {
			continue
		}
		if len(b.hits) == 0 || !b.hits[len(b.hits)-1].at.After(cutoff) {
			if !b.blockedUntil.IsZero() {
				sb.info("block expired for subnet=%s", k)
			}
			delete(s.buckets, k)
			removed++
		}
	}
	if removed > 0 {
		sb.debug("sweep removed %d idle subnets, %d still tracked", removed, len(s.buckets))
	}
}

func (sb *SubnetBurst) counts(path string) bool {
	if len(sb.cfg.CountPaths) == 0 {
		return true
	}
	for _, p := range sb.cfg.CountPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (sb *SubnetBurst) clientIP(req *http.Request) (netip.Addr, bool) {
	remote, ok := parseAddr(req.RemoteAddr)
	if sb.cfg.IPHeader == "" {
		return remote, ok
	}
	if len(sb.trusted) > 0 && (!ok || !contains(sb.trusted, remote)) {
		sb.debug("ignoring %s header: remoteAddr=%q is not in trustedProxies", sb.cfg.IPHeader, req.RemoteAddr)
		return remote, ok
	}
	v := req.Header.Get(sb.cfg.IPHeader)
	// For list headers like X-Forwarded-For, the last entry was added by the nearest proxy.
	if i := strings.LastIndex(v, ","); i >= 0 {
		v = v[i+1:]
	}
	if addr, ok := parseAddr(strings.TrimSpace(v)); ok {
		return addr, true
	}
	sb.debug("falling back to remoteAddr=%q: %s header is missing or invalid (%q)", req.RemoteAddr, sb.cfg.IPHeader, v)
	return remote, ok
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port".
func parseAddr(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

func parsePrefixes(raw []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if !strings.Contains(r, "/") {
			a, err := netip.ParseAddr(r)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func unit(distinct bool) string {
	if distinct {
		return "distinct IPs"
	}
	return "requests"
}

func hitIPs(hits []hit) string {
	ips := make([]string, len(hits))
	for i, h := range hits {
		ips[i] = h.addr.String()
	}
	return strings.Join(ips, ",")
}

// Plugins don't inherit Traefik's log level or format, so lines imitate Traefik's own output.
func (sb *SubnetBurst) logf(level, format string, v ...any) {
	sb.logger.Printf("%s %s middlewareName=%s msg=%q", time.Now().UTC().Format(time.RFC3339), level, sb.name, fmt.Sprintf(format, v...))
}

func (sb *SubnetBurst) info(format string, v ...any) { sb.logf("INF", format, v...) }

// Arguments are handled in the manner of [fmt.Printf].
func (sb *SubnetBurst) debug(format string, v ...any) {
	if sb.cfg.Debug {
		sb.logf("DBG", format, v...)
	}
}
