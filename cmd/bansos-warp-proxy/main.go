// Command bansos-warp-proxy is a WARP-egress relay/proxy for
// bansos-router. It accepts bansos relay requests (x-relay-target /
// x-relay-path) and standard CONNECT requests, forwards them through a
// userspace Cloudflare WARP pool, and streams the response back.
//
// No Cloudflare account is required: each pool exit registers itself
// anonymously via the Cloudflare client API at startup (see
// internal/warp). If WARP is enabled and the first registration fails,
// the server refuses to start rather than serving traffic from the
// VPS IP directly (fail-closed).
//
// Configuration: -config <path> (JSON, optional) plus environment
// overrides documented in internal/config. Secrets come only from
// the environment, never from the JSON file.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nurhamidqq/bansos-warp-proxy/internal/config"
	"github.com/nurhamidqq/bansos-warp-proxy/internal/proxy"
	"github.com/nurhamidqq/bansos-warp-proxy/internal/warp"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config.json (optional; defaults apply when missing)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var dialer proxy.Dialer
	var pool *warp.Pool
	if cfg.WarpEnabled {
		stack, err := warp.ParseStack(cfg.WarpStack)
		if err != nil {
			log.Fatalf("warp stack: %v", err)
		}
		var source netip.Prefix
		if cfg.WarpSourceIPv6 != "" {
			source, err = netip.ParsePrefix(cfg.WarpSourceIPv6)
			if err != nil {
				log.Fatalf("warp_source_ipv6: %v", err)
			}
			source = source.Masked()
		}
		// Give the first registration room for one full Cloudflare
		// rate-limit window instead of failing startup on a pause
		// that is about to end.
		timeout := time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second
		if timeout < warp.RegistrationWindow+30*time.Second {
			timeout = warp.RegistrationWindow + 30*time.Second
		}
		regCtx, regCancel := context.WithTimeout(ctx, timeout)
		pool, err = warp.NewWithOptions(regCtx, warp.Options{
			Size:       cfg.WarpPoolSize,
			Stack:      stack,
			SourceIPv6: source,
		})
		regCancel()
		if err != nil {
			log.Fatalf("initialize WARP egress: %v (refusing to start; fix network/UDP 2408 access to Cloudflare, do not bypass by serving direct traffic)", err)
		}
		defer pool.Close()
		dialer = proxy.NewWarpDialer(pool, time.Duration(cfg.UpstreamTimeoutSeconds)*time.Second)
		snap := pool.Snapshot()
		log.Printf("warp pool ready: size=%d active=%d egress=%v", snap.Size, snap.Active, snap.Egress)
	} else {
		log.Printf("WARNING: warp_enabled=false, traffic leaves from the VPS IP directly (testing only)")
		dialer = proxy.NewDirectDialer(time.Duration(cfg.UpstreamTimeoutSeconds) * time.Second)
	}

	srv := proxy.New(cfg, dialer)
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("bansos-warp-proxy listening on %s (warp=%v, allowed_targets=%v)\n", cfg.Listen, cfg.WarpEnabled, cfg.AllowedTargets)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server stopped: %v", err)
	}
}
