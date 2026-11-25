# make link    use this checkout's build as `hydra` while developing
# make unlink  go back to the Homebrew build
LINK := $(HOME)/.local/bin/hydra

.PHONY: build test test-tui link unlink

build: cmd/hydra/dashboard.mjs
	go build -o bin/ ./cmd/...

# The dashboard (bare `hydra`) is an Ink app in tui/, bundled into
# cmd/hydra/dashboard.mjs, which is committed: go build needs no npm. This
# rebuilds it when its sources are newer; commit the result with them.
TUI_SRC := $(shell find tui/src -type f) tui/package.json tui/package-lock.json tui/build.mjs tui/tsconfig.json
cmd/hydra/dashboard.mjs: $(TUI_SRC)
	@if ! command -v npm >/dev/null; then echo "npm isn't installed: building with the committed dashboard"; exit 0; fi; \
	cd tui && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run --silent build

test:
	go test ./...

test-tui:
	cd tui && { [ -d node_modules ] || npm ci --no-audit --no-fund; } && npm run --silent typecheck && npm test --silent && npm run --silent check

# ~/.local/bin comes after /opt/homebrew/bin on PATH, so Homebrew's copy is
# unlinked while the checkout's is in use. `make build` refreshes it; the
# symlink keeps pointing at bin/hydra.
link: build
	-brew unlink santiagotoscanini/tap/hydra 2>/dev/null
	mkdir -p $(dir $(LINK))
	ln -sfn $(CURDIR)/bin/hydra $(LINK)
	@echo "hydra -> $$(command -v hydra)"

# Re-linking Homebrew's copy shadows the symlink, which can stay.
unlink:
	-brew link santiagotoscanini/tap/hydra 2>/dev/null
	@echo "hydra -> $$(command -v hydra || echo 'not installed')"
