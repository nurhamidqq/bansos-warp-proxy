// Package warp carries outbound TCP traffic through Cloudflare WARP using
// a userspace WireGuard device and a gVisor network stack. It needs no TUN
// device, root privileges, or system routes: UDP sockets carry WireGuard
// packets, and the netstack terminates TCP inside the process.
//
// The design follows opencode2api's internal/warp package (GPL-3.0-or-later),
// which itself notes it is adapted from github.com/sudogeeker/ai-route
// (MIT). This package is a clean-room-style reimplementation of the same
// protocol, not a verbatim copy: registration, exit verification, and a
// small rotating pool are the only parts kept, because they are the parts
// a bansos relay needs.
//
// Registration is anonymous and automatic: no Cloudflare account, no
// warp-cli, and no WARP+ subscription is required. Each pool exit registers
// itself as a fresh WARP device via the Cloudflare client API (see
// registrar.go), then builds one userspace tunnel from the returned config.
// A pool exit that the upstream rejects (HTTP 429, or 403) is retired and
// replaced in the background; its public egress block is cooled for
// 30 minutes so a replacement is a real rotation, not a cosmetic address
// change.
//
// SPDX-License-Identifier: GPL-3.0-or-later
package warp
