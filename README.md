# bansos-warp-proxy

Proxy egress WARP untuk **bansos-router** (fork `nurhamidqq/bansos-router`).
Server ini menerima request relay bansos, meneruskannya ke upstream
lewat tunnel Cloudflare WARP userspace, lalu streaming responsnya balik.

> **Status jujur:** `go vet ./...` dan `go build ./...`
> **lolos** di sini (Go 1.24 diunduh khusus untuk tes ini), dan
> mode direct sudah dites: `/healthz` 200, relay ke target di
> luar allowlist 403, relay ke target allowlist lolos allowlist,
> CONNECT ke alamat privat 403, CONNECT ke host publik 200.
> Satu bug ketemu saat tes dan sudah diperbaiki: `http.ServeMux`
> me-redirect CONNECT jadi 301, jadi routing sekarang manual,
> bukan ServeMux. **Yang belum teruji live adalah registrasi
> WARP-nya:** dari VM ini registrasi gagal fail-closed
> (`tls: first record does not look like a TLS handshake`)
> karena egress VM ini mencegat TLS langsung — itu perilaku
> yang benar di sini, tapi artinya tes WARP pertama harus
> dilakukan di VPS yang internetnya langsung, bukan dari VM
> ini. Jangan pakai mode direct untuk pemakaian nyata.

## Ini apa, dan apa bukan

- **Ini:** satu binary Go yang jadi egress bansos. Bansos tetap yang
  mengatur model, failover, dan dashboard. Server ini cuma
  mengganti IP keluarnya.
- **Bukan:** gateway bansos, bukan open proxy umum, bukan VPN
  sistem. Target relay dibatasi ke 3 upstream bansos saja.
- **Bukan:** jalan pintas untuk memakai model berbayar gratis.
  Upstream yang dipakai tetap jalur gratis yang sama, cuma
  egress-nya dirotasi lewat WARP.

## Cara kerja, singkat

```
bansos (laptop) --relay--> VPS:18080 --WARP tunnel--> opencode.ai
     x-relay-target: https://opencode.ai
     x-relay-path:   /v1/chat/completions
```

1. Bansos kirim request ke URL relay dengan method/body asli,
   plus header `x-relay-target` (origin) dan `x-relay-path`
   (path+query). Ini protokol yang sudah ada di
   `bansos-router/src/relay/egress.ts`.
2. Server ini validasi origin ke allowlist, validasi IP sumber,
   hapus header kontrol, lalu forward lewat satu lease WARP.
3. Lease dipegang selama respons di-stream (SSE tetap jalan),
   lalu dilepas. Kalau upstream balas 429/403, exit itu
   di-retire dan diganti di background, blok egress-nya
   di-cooling 30 menit. Request berikutnya dapat exit baru.
4. Kalau tidak ada exit WARP yang bisa dipakai, server jawab
   503. **Tidak ada fallback diam-diam ke direct** — fallback
   seperti itu akan membocorkan IP yang sedang dihindari.

WARP di sini userspace: WireGuard + gVisor netstack di dalam
proses, tanpa TUN, tanpa root, tanpa mengubah routing sistem.
Registrasi device WARP-nya anonim dan otomatis (lihat bawah),
tidak perlu akun Cloudflare.

## Prasyarat

- VPS Ubuntu/Debian, bisa keluar HTTPS 443 ke
  `api.cloudflareclient.com` dan UDP 2408 ke endpoint Cloudflare
  (`engage.cloudflareclient.com`, `162.159.192.1`, dll).
- Go 1.24 di VPS (script install akan memasangnya kalau belum ada).
- IP publik mesin yang menjalankan bansos (laptop), **hanya**
  kalau kamu pakai mode port publik (mode B di bawah).

### Apakah harus daftar Cloudflare WARP dulu?

**Tidak.** Tidak perlu akun, tidak perlu `warp-cli`, tidak perlu
WARP+. Tiap exit pool mendaftarkan dirinya sendiri:

1. Generate keypair WireGuard X25519.
2. `POST https://api.cloudflareclient.com/v0i1909051800/reg`
   dengan public key itu.
3. `PATCH /reg/{id}` dengan `{"warp_enabled": true}`.
4. `GET /reg/{id}` untuk ambil alamat tunnel dan peer endpoint.
5. Bangun tunnel, probe lewat `https://1.1.1.1/cdn-cgi/trace`
   (atau IPv6-nya), pastikan `warp=on`.

Kalau Cloudflare balas 429, registrasi dijeda sesuai
`Retry-After` (default 12 detik, maks 5 menit), bukan di-hammer.
Kalau registrasi pertama gagal, server tidak jadi start.

## Instalasi di VPS

```bash
git clone https://github.com/nurhamidqq/bansos-warp-proxy.git
cd bansos-warp-proxy
sudo ./scripts/install-vps.sh
```

Lalu pilih salah satu mode:

### Mode A — SSH tunnel (disarankan, tidak ada port publik)

Di VPS, biarkan proxy bind loopback saja:

```json
{
  "listen": "127.0.0.1:18080",
  "allowed_source_ips": [],
  "warp_enabled": true,
  "warp_pool_size": 2,
  "warp_stack": "ipv6"
}
```

Di laptop, buat tunnel setiap kali mau pakai bansos:

```bash
ssh -N -L 18080:127.0.0.1:18080 <user>@<VPS_IP>
```

Keuntungan: tidak ada port yang terekspos ke internet sama
sekali, tidak perlu IP allowlist (IP laptop yang dinamis pun
tidak masalah), dan request relay tidak lewat HTTP polos di
internet — dia lewat SSH.

### Mode B — Port publik + IP allowlist + firewall

Di `/opt/bansos-warp-proxy/config.json`:

```json
{
  "listen": "0.0.0.0:18080",
  "allowed_source_ips": ["<IP_PUBLIK_LAPTOP>"],
  "warp_enabled": true,
  "warp_pool_size": 2,
  "warp_stack": "ipv6"
}
```

Lalu kunci di firewall VPS (contoh ufw):

```bash
ufw allow from <IP_PUBLIK_LAPTOP> to any port 18080 proto tcp
# pastikan default ufw sudah deny untuk port lain / port ini dari IP lain
```

Server **menolak start** kalau `listen` bukan loopback tapi
`allowed_source_ips` kosong. Itu sengaja, supaya tidak ada
relay publik yang tidak sengaja terbuka. Catatan: relay bansos
tidak bisa membawa token (protokolnya tidak punya tempat
untuk itu), jadi IP allowlist + firewall adalah autentikasi
relay-nya. Token di server ini hanya berlaku untuk CONNECT
(lihat bawah), bukan untuk relay.

Kalau mau HTTPS untuk mode B, pasang Caddy/Nginx TLS di depan
port ini dan arahkan relay URL ke `https://...` itu. Tanpa TLS,
isi request relay (termasuk prompt) lewat HTTP polos — jangan
pakai mode B polos untuk prompt yang sensitif.

Setelah config diubah:

```bash
systemctl restart bansos-warp-proxy
journalctl -u bansos-warp-proxy -f
curl http://127.0.0.1:18080/healthz   # dari VPS, atau lewat tunnel
```

`healthz` menampilkan `egress` — daftar IP publik WARP yang
aktif. Itulah IP yang akan dilihat upstream, bukan IP VPS.

Token CONNECT (opsional, hanya untuk CONNECT):

```bash
# di /etc/bansos-warp-proxy.env (file ini tidak di-commit)
BANSOS_WARP_TOKEN=isi-token-panjang-di-sini
systemctl restart bansos-warp-proxy
```

## Hubungkan ke bansos-router

Di mesin yang menjalankan bansos (laptop), pastikan strict
mode **mati** — di strict mode bansos menonaktifkan relay
total. Lalu:

```bash
# Mode A (SSH tunnel):
bansos relay url http://127.0.0.1:18080
bansos relay use http://127.0.0.1:18080
bansos relay on

# Mode B (port publik):
bansos relay url http://<VPS_IP>:18080
bansos relay use http://<VPS_IP>:18080
bansos relay on

bansos relay status
bansos doctor
bansos ping
```

Penjelasan perintah: `url` menyimpan relay ke daftar,
`use` mengaktifkan relay itu, `on` mengaktifkan egress relay.
Cek probe-nya: bansos melakukan GET polos ke URL relay dan
menganggap `ok` kalau status < 500. Server ini menjawab GET
polos dengan `healthz` 200, jadi probe harus `ok: true`.

Lalu kirim satu request chat lewat bansos seperti biasa dan
lihat log VPS: baris `relay status=... target=... egress=...`
menunjukkan exit mana yang dipakai. Bandingkan egress itu
dengan IP laptop/VPS — harus berbeda dari keduanya.

### Peringatan fallback bansos (penting)

`relayFetch` bansos sekarang: kalau request ke relay **gagal
di level jaringan** (misal server mati, tunnel putus), dia
fallback ke direct dari IP laptop. Itu perilaku bansos, bukan
server ini, dan tidak bisa diperbaiki dari sisi server.
Artinya: kalau server relay mati, bansos tidak berhenti —
dia jalan direct. Pantau `healthz` dan log, dan jangan
menganggap semua request selalu lewat WARP kalau server
sedang down. Perbaikan fail-closed di sisi bansos adalah
pekerjaan lanjutan yang terpisah.

## Tes manual (tanpa bansos)

Relay, dari laptop (lewat tunnel atau IP yang di-allowlist):

```bash
curl -s -X POST http://127.0.0.1:18080/ \
  -H "x-relay-target: https://opencode.ai" \
  -H "x-relay-path: /v1/models" \
  -H "Authorization: Bearer public"
```

CONNECT, dengan token (ganti token dan VPS IP):

```bash
curl -s -x http://user:<TOKEN>@<VPS_IP>:18080 https://api.ipify.org
# harusnya mengembalikan IP egress WARP, bukan IP VPS
```

## Konfigurasi lengkap

File JSON (`config.json`, opsional — kalau tidak ada, pakai
default loopback) plus environment override:

| Field | Default | Keterangan |
|---|---|---|
| `listen` | `127.0.0.1:18080` | Bind address |
| `allowed_source_ips` | `[]` | IP/CIDR yang boleh pakai relay. Wajib diisi kalau listen publik |
| `allowed_targets` | 3 upstream bansos | Allowlist origin relay |
| `warp_enabled` | `true` | `false` = direct dari IP VPS, hanya untuk tes |
| `warp_pool_size` | `2` | Maks 8, supaya tidak kena rate limit registrasi |
| `warp_stack` | `ipv6` | `ipv6` disarankan; egress IPv4 WARP itu NAT bersama dan kuotanya gampang habis |
| `warp_source_ipv6` | `""` | Prefix /64 atau lebih pendek yang di-route ke host, untuk rotasi egress yang benar-benar baru |
| `max_conns` | `50` | Batas request bersamaan |
| `upstream_timeout_seconds` | `120` | Timeout per request upstream |

Environment: `BANSOS_WARP_LISTEN`, `BANSOS_WARP_TOKEN`,
`BANSOS_WARP_ALLOWED_IPS` (pisah koma), `BANSOS_WARP_POOL_SIZE`,
`BANSOS_WARP_STACK`, `BANSOS_WARP_SOURCE_IPV6`,
`BANSOS_WARP_ENABLED=false` untuk mode direct. Token **tidak
pernah** dibaca dari JSON, hanya dari environment.

## Troubleshooting

- **Server tidak start, error WARP:** cek UDP 2408 keluar dari
  VPS (firewall provider VPS sering memblokir UDP), dan HTTPS
  ke `api.cloudflareclient.com`. Kalau registrasi pertama
  gagal, server sengaja tidak start — jangan bypass dengan
  `warp_enabled=false` untuk pemakaian nyata.
- **Registrasi 429:** tunggu. Pool akan menjeda sesuai
  `Retry-After`. Jangan naikkan pool size untuk "mempercepat",
  itu justru memperparah rate limit.
- **Relay 403 `source not allowed`:** IP laptop berubah
  (IP dinamis). Update `allowed_source_ips` + firewall, atau
  pindah ke mode SSH tunnel yang tidak bergantung IP.
- **Relay 403 `relay target not allowed`:** origin di luar
  allowlist. Tambahkan ke `allowed_targets` hanya kalau kamu
  yakin origin itu upstream bansos yang sah.
- **Probe bansos `ok: false`:** server tidak bisa dijangkau
  dari laptop (tunnel belum jalan / firewall / port salah),
  bukan masalah WARP.
- **SSE/streaming putus:** server sudah flush per chunk.
  Kalau putus, cek timeout upstream dan log `relay copy error`.

## Batasan v1

- Rotasi terjadi saat upstream balas 429/403 untuk satu
  request. Server ini tidak me-retry otomatis request yang
  sama lewat exit baru di dalam satu relay call — retry itu
  urusan bansos (failover) atau klien. Ini disengaja supaya
  satu request tidak pernah dikirim dua kali ke upstream
  tanpa sepengetahuan pemanggil.
- Tidak ada dashboard WebUI. Pantau lewat `healthz` dan
  `journalctl`.
- Tidak ada persistensi registrasi: tiap restart, exit
  diregistrasi ulang. Itu disengaja untuk rotasi, bukan bug.

## Lisensi

GPL-3.0-or-later (lihat `LICENSE`). Desain `internal/warp`
mengikuti `opencode2api/internal/warp` (GPL), yang sendiri
diadaptasi dari `sudogeeker/ai-route` (MIT). Protokol relay
yang diimplementasikan adalah milik bansos-router (GPL-3.0).
