// Package proxy implements the two frontends bansos-router can use:
//
//   - Relay: bansos-router's own protocol (src/relay/egress.ts). Bansos
//     fetches the relay URL with the original method/body, plus headers
//     x-relay-target (upstream origin) and x-relay-path (path+query).
//     This server reconstructs target = origin + path, forwards the
//     request through a WARP lease, and streams the response back with
//     its status and headers intact. A plain GET without x-relay-target
//     (bansos's relay probe, and /healthz) returns 200, not an error,
//     because the probe treats any status < 500 as "ok".
//
//   - CONNECT: a standard forward proxy. The client sends
//     CONNECT host:port, this server dials that host through a WARP
//     lease, answers 200, then pumps bytes both ways without ever
//     terminating TLS. This is for curl tests and for a future bansos
//     egress-pool feature; bansos relay today uses the relay endpoint.
//
// Security, in order:
//  1. Relay targets are restricted to the configured allowlist
//     (default: the three bansos upstreams). No other origin can be
//     reached through the relay, and loopback/private targets are
//     impossible because they are not on the list.
//  2. Relay requests from a non-loopback source must be in
//     AllowedSourceIPs. Bansos relay cannot send a token, so this
//     plus the host firewall is the relay's authentication.
//  3. CONNECT requires the configured token when one is set
//     (Proxy-Authorization Basic with password = token, or the
//     X-Warp-Token header); when no token is set it requires a
//     source in AllowedSourceIPs. CONNECT to loopback/private
//     destinations is always refused, so the proxy cannot be used
//     for SSRF.
//  4. Hop-by-hop headers, Proxy-Authorization, and x-relay-* /
//     x-warp-* headers are stripped before forwarding. Authorization
//     and other upstream headers are forwarded untouched, because
//     Zen needs them, but they are never logged.
//  5. Bodies are streamed, never buffered whole, so SSE / streaming
//     chat responses work and memory stays bounded. The WARP lease
//     is held for the whole response and released when the body
//     closes, so a pooled connection can never outlive its exit.
//     Reporting a 429/403 retires the exit for future requests but
//     does not kill the in-flight stream, because the pool only
//     closes a retired exit once its refcount reaches zero.
//  6. Fail-closed: if no WARP exit can be acquired, the server
//     answers 503. It never falls back to a direct dial from the
//     VPS IP, because that would silently leak the egress the user
//     is trying to rotate away from.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nurhamidqq/bansos-warp-proxy/internal/config"
	"github.com/nurhamidqq/bansos-warp-proxy/internal/warp"
)

// Session is one egress acquisition: an HTTP client dialling through
// it, a raw dial function for CONNECT, and the hooks to report the
// outcome back to the pool. Release must be called exactly once,
// after the response body is fully read or abandoned.
type Session struct {
	Client  *http.Client
	Dial    func(ctx context.Context, network, address string) (net.Conn, error)
	Egress  string
	Release func()
	Report  func(status int)
	Fail    func(reason string)
}

// Dialer abstracts the egress: WARP pool, or direct for testing.
type Dialer interface {
	Dial(ctx context.Context) (*Session, error)
	Snapshot() any
}

type warpDialer struct {
	pool    *warp.Pool
	timeout time.Duration
}

func NewWarpDialer(pool *warp.Pool, timeout time.Duration) Dialer {
	return &warpDialer{pool: pool, timeout: timeout}
}

func (d *warpDialer) Dial(ctx context.Context) (*Session, error) {
	lease, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	dial := func(ctx context.Context, _, address string) (net.Conn, error) {
		return lease.DialContext(ctx, lease.DialNetwork(), address)
	}
	transport := &http.Transport{
		Proxy:           nil,
		DialContext:     dial,
		ForceAttemptHTTP2: false,
		// A clone of DefaultTransport after its first dial inherits
		// HTTP/2 ALPN/TLSNextProto, which breaks HTTP/1.1 readers.
		// Pin HTTP/1.1 explicitly, like opencode2api does.
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:     &tls.Config{NextProtos: []string{"http/1.1"}},
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: d.timeout,
	}
	return &Session{
		Client: &http.Client{Transport: transport},
		Dial:   dial,
		Egress: lease.Egress(),
		Release: func() {
			transport.CloseIdleConnections()
			lease.Release()
		},
		Report: func(status int) { d.pool.Report(lease, status) },
		Fail:   func(reason string) { d.pool.Fail(lease, reason) },
	}, nil
}

func (d *warpDialer) Snapshot() any { return d.pool.Snapshot() }

type directDialer struct {
	client *http.Client
	dialer *net.Dialer
}

func NewDirectDialer(timeout time.Duration) Dialer {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &directDialer{
		dialer: dialer,
		client: &http.Client{Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		}},
	}
}

func (d *directDialer) Dial(_ context.Context) (*Session, error) {
	noop := func() {}
	return &Session{
		Client:  d.client,
		Dial:    d.dialer.DialContext,
		Egress:  "direct",
		Release: noop,
		Report:  func(int) {},
		Fail:    func(string) {},
	}, nil
}

func (d *directDialer) Snapshot() any { return nil }

type Server struct {
	cfg    *config.Config
	dialer Dialer
	sem    chan struct{}
}

func New(cfg *config.Config, dialer Dialer) *Server {
	return &Server{cfg: cfg, dialer: dialer, sem: make(chan struct{}, cfg.MaxConns)}
}

// Handler returns a plain handler, deliberately NOT http.ServeMux:
// ServeMux redirects CONNECT requests (whose URL path is empty) to
// "/" with a 301, which breaks proxy clients. Routing here is manual
// and only distinguishes /healthz from everything else.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			s.handleHealth(w, r)
			return
		}
		s.handle(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"warp_enabled": s.cfg.WarpEnabled,
		"egress":       s.dialer.Snapshot(),
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	// Plain GET without relay headers is the bansos relay probe
	// (and a human health check): answer 200, not 400/404.
	if r.Header.Get("x-relay-target") == "" {
		if r.Method == http.MethodGet {
			s.handleHealth(w, r)
			return
		}
		http.Error(w, "missing x-relay-target", http.StatusBadRequest)
		return
	}
	s.handleRelay(w, r)
}

// handleRelay implements bansos-router's relay protocol exactly:
// target = x-relay-target (origin) + x-relay-path (path+query),
// method and body are the original request's.
func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.SourceAllowed(remoteIP(r)) {
		http.Error(w, "source not allowed", http.StatusForbidden)
		return
	}
	targetOrigin := strings.TrimSpace(r.Header.Get("x-relay-target"))
	targetPath := r.Header.Get("x-relay-path")
	if targetOrigin == "" || targetPath == "" || !strings.HasPrefix(targetPath, "/") {
		http.Error(w, "missing or invalid x-relay-target/x-relay-path", http.StatusBadRequest)
		return
	}
	allowed := false
	for _, t := range s.cfg.AllowedTargets {
		if t == targetOrigin {
			allowed = true
			break
		}
	}
	if !allowed {
		http.Error(w, "relay target not allowed", http.StatusForbidden)
		return
	}
	targetURL := targetOrigin + targetPath
	if _, err := url.Parse(targetURL); err != nil {
		http.Error(w, "invalid relay target", http.StatusBadRequest)
		return
	}
	s.forward(w, r, targetURL)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Token != "" {
		if !s.checkToken(r) {
			w.Header().Set("Proxy-Authenticate", "Basic realm=\"bansos-warp-proxy\"")
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
	} else if !s.cfg.SourceAllowed(remoteIP(r)) {
		http.Error(w, "source not allowed", http.StatusForbidden)
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		http.Error(w, "CONNECT to private address refused", http.StatusForbidden)
		return
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "proxy busy", http.StatusServiceUnavailable)
		return
	}
	sess, err := s.dialer.Dial(r.Context())
	if err != nil {
		http.Error(w, "no egress available", http.StatusServiceUnavailable)
		return
	}
	defer sess.Release()
	targetConn, err := sess.Dial(r.Context(), "tcp", r.Host)
	if err != nil {
		sess.Fail("connect dial failed")
		http.Error(w, "CONNECT failed", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, buf, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n\r\n")
	_ = buf.Flush()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(targetConn, clientConn) }()
	go func() { defer wg.Done(); _, _ = io.Copy(clientConn, targetConn) }()
	wg.Wait()
}

func (s *Server) checkToken(r *http.Request) bool {
	if h := r.Header.Get("X-Warp-Token"); h != "" {
		return h == s.cfg.Token
	}
	auth := r.Header.Get("Proxy-Authorization")
	if strings.HasPrefix(auth, "Basic ") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if err == nil {
			// Basic user:pass; the token is the password, user ignored.
			if _, pass, ok := strings.Cut(string(decoded), ":"); ok && pass == s.cfg.Token {
				return true
			}
		}
	}
	return false
}

// forward streams one relay request through one egress session.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, targetURL string) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		http.Error(w, "proxy busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.cfg.UpstreamTimeoutSeconds)*time.Second)
	defer cancel()
	sess, err := s.dialer.Dial(ctx)
	if err != nil {
		http.Error(w, "no egress available", http.StatusServiceUnavailable)
		return
	}
	defer sess.Release()

	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "invalid upstream request", http.StatusBadRequest)
		return
	}
	copyHeaders(upReq.Header, r.Header)
	// Never forward relay/proxy control headers, and never let a
	// caller smuggle a Host that does not match the target.
	upReq.Header.Del("x-relay-target")
	upReq.Header.Del("x-relay-path")
	upReq.Header.Del("x-warp-token")
	upReq.Header.Del("Proxy-Authorization")
	upReq.Header.Del("Proxy-Connection")
	upReq.Header.Del("Connection")
	upReq.Header.Del("Keep-Alive")
	upReq.Header.Del("Te")
	upReq.Header.Del("Trailer")
	upReq.Header.Del("Transfer-Encoding")
	upReq.Header.Del("Upgrade")
	if upReq.URL.Host != "" {
		upReq.Host = upReq.URL.Host
	}

	resp, err := sess.Client.Do(upReq)
	if err != nil {
		sess.Fail("upstream dial failed")
		log.Printf("relay upstream error target=%s egress=%s err=%v", targetURL, sess.Egress, err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Report the status now, before streaming the body. Reporting
	// retires a 429/403 exit for future requests, but does not kill
	// this stream: the pool only closes a retired exit once its
	// refcount reaches zero, which happens at sess.Release().
	sess.Report(resp.StatusCode)
	log.Printf("relay status=%d target=%s egress=%s", resp.StatusCode, targetURL, sess.Egress)

	copyHeaders(w.Header(), resp.Header)
	w.Header().Del("Connection")
	w.Header().Del("Keep-Alive")
	w.Header().Del("Transfer-Encoding")
	w.WriteHeader(resp.StatusCode)
	_, copyErr := io.Copy(&flushWriter{w: w}, resp.Body)
	if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
		log.Printf("relay copy error target=%s err=%v", targetURL, copyErr)
	}
}

type flushWriter struct {
	w http.ResponseWriter
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
