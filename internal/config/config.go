// Package config loads bansos-warp-proxy configuration from a JSON file
// (if present) plus environment overrides. Secrets are never read from the
// JSON file: the CONNECT token, when used, comes only from the environment.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// DefaultAllowedTargets mirrors bansos-router's ALLOWED_TARGETS in
// src/relay/egress.ts. A relay request to any other origin is rejected, so
// this server can never be abused as a general-purpose open proxy.
var DefaultAllowedTargets = []string{
	"https://opencode.ai",
	"https://api.kilo.ai",
	"https://api.llm7.io",
}

type Config struct {
	// Listen is the address the proxy binds, e.g. "127.0.0.1:18080".
	// On a VPS this is usually "0.0.0.0:18080" combined with
	// AllowedSourceIPs (see Validate) and a host firewall.
	Listen string `json:"listen"`

	// Token, when non-empty, is required for CONNECT requests via
	// Proxy-Authorization (Basic, password = token) or the
	// X-Warp-Token header. It is deliberately NOT used for relay
	// requests: bansos-router's relayFetch cannot attach a custom
	// token, so relay requests are protected by AllowedSourceIPs
	// plus the host firewall instead. Never put a token in JSON.
	Token string `json:"-"`

	// AllowedSourceIPs is the app-level IP allowlist for relay
	// requests. Empty means: loopback only. A non-loopback Listen
	// without this list is refused at startup (see Validate).
	AllowedSourceIPs []string `json:"allowed_source_ips"`

	// AllowedTargets is the relay allowlist. Defaults to the three
	// bansos upstreams above.
	AllowedTargets []string `json:"allowed_targets"`

	// WarpEnabled selects the egress dialer. true = userspace WARP
	// pool (default, recommended). false = direct dial from the VPS
	// IP, for testing only: it defeats the anti-block purpose.
	WarpEnabled bool `json:"warp_enabled"`

	WarpPoolSize int    `json:"warp_pool_size"`
	WarpStack    string `json:"warp_stack"` // "ipv6" (default), "ipv4", "dual"
	// WarpSourceIPv6, when set, must be an IPv6 prefix /64 or
	// shorter routed to this host. Each exit draws its own outer
	// source address from it, so replacements get a genuinely new
	// public egress. Empty = Cloudflare assigns the egress.
	WarpSourceIPv6 string `json:"warp_source_ipv6"`

	MaxConns               int `json:"max_conns"`
	UpstreamTimeoutSeconds int `json:"upstream_timeout_seconds"`
}

func Default() *Config {
	return &Config{
		Listen:                 "127.0.0.1:18080",
		AllowedTargets:         append([]string(nil), DefaultAllowedTargets...),
		WarpEnabled:            true,
		WarpPoolSize:           2,
		WarpStack:              "ipv6",
		MaxConns:               50,
		UpstreamTimeoutSeconds: 120,
	}
}

// Load reads path if it exists, then applies environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config %s: %w", path, err)
			}
		} else if len(data) > 0 {
			if err := json.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}
	applyEnv(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("BANSOS_WARP_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("BANSOS_WARP_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("BANSOS_WARP_ALLOWED_IPS"); v != "" {
		var ips []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				ips = append(ips, p)
			}
		}
		cfg.AllowedSourceIPs = ips
	}
	if v := os.Getenv("BANSOS_WARP_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WarpPoolSize = n
		}
	}
	if v := os.Getenv("BANSOS_WARP_STACK"); v != "" {
		cfg.WarpStack = v
	}
	if v := os.Getenv("BANSOS_WARP_SOURCE_IPV6"); v != "" {
		cfg.WarpSourceIPv6 = v
	}
	if v := os.Getenv("BANSOS_WARP_ENABLED"); v == "false" || v == "0" {
		cfg.WarpEnabled = false
	}
}

func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate applies the fail-closed rules. In particular it refuses to
// start a public relay with no source allowlist: bansos relay has no
// token mechanism, so an unguarded public relay would be usable by
// anyone who finds the port, limited only by the target allowlist.
func (cfg *Config) Validate() error {
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", cfg.Listen, err)
	}
	if cfg.WarpPoolSize < 1 {
		cfg.WarpPoolSize = 1
	}
	if cfg.WarpPoolSize > 8 {
		return fmt.Errorf("warp_pool_size %d is too large; keep it at 8 or below to avoid Cloudflare registration rate limits", cfg.WarpPoolSize)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.WarpStack)) {
	case "", "ipv6", "ipv4", "dual":
	default:
		return fmt.Errorf("warp_stack %q: use ipv6, ipv4 or dual", cfg.WarpStack)
	}
	if len(cfg.AllowedTargets) == 0 {
		cfg.AllowedTargets = append([]string(nil), DefaultAllowedTargets...)
	}
	for _, ip := range cfg.AllowedSourceIPs {
		if net.ParseIP(ip) == nil {
			if _, _, err := net.ParseCIDR(ip); err != nil {
				return fmt.Errorf("allowed_source_ips %q is not an IP or CIDR", ip)
			}
		}
	}
	if cfg.MaxConns < 1 {
		cfg.MaxConns = 50
	}
	if cfg.UpstreamTimeoutSeconds < 10 {
		cfg.UpstreamTimeoutSeconds = 120
	}
	if !isLoopbackHost(host) && len(cfg.AllowedSourceIPs) == 0 {
		return fmt.Errorf("listen %q is not loopback but allowed_source_ips is empty: refusing to start an unguarded public relay; set allowed_source_ips to your bansos machine's public IP (and firewall the port to the same IP)", cfg.Listen)
	}
	return nil
}

// SourceAllowed reports whether remoteIP may use the relay endpoint.
func (cfg *Config) SourceAllowed(remoteIP string) bool {
	ip := net.ParseIP(remoteIP)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, entry := range cfg.AllowedSourceIPs {
		if parsed := net.ParseIP(entry); parsed != nil {
			if parsed.Equal(ip) {
				return true
			}
			continue
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
			return true
		}
	}
	return false
}
