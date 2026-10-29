GO ?= go
ADDR ?= 127.0.0.1:8080

# terminal-to-html v3.17+ requires Go 1.25. v3.16.0 is pinned so this builds on
# Go 1.22+; raise both together if you are on a newer toolchain.
export GOFLAGS := -mod=mod

.PHONY: run build test assets tidy clean

run: assets
	$(GO) run ./cmd/streamterm -addr $(ADDR)

build: assets
	$(GO) build -o bin/streamterm ./cmd/streamterm

test:
	$(GO) test ./...

assets: web/vendor/terminal.css

web/vendor/terminal.css: go.mod tools/fetch-assets.sh
	./tools/fetch-assets.sh

tidy:
	$(GO) mod tidy

clean:
	rm -rf bin web/vendor/terminal.css
