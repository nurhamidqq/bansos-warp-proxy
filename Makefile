.PHONY: build vet clean run

build:
	go build -trimpath -o bansos-warp-proxy ./cmd/bansos-warp-proxy

vet:
	go vet ./...

clean:
	rm -f bansos-warp-proxy

# Local test only: direct dialer, loopback, no WARP, no config file
# (a missing config file means defaults: 127.0.0.1:18080).
run:
	BANSOS_WARP_LISTEN=127.0.0.1:18080 BANSOS_WARP_ENABLED=false go run ./cmd/bansos-warp-proxy -config /tmp/bansos-warp-proxy-no-config.json
