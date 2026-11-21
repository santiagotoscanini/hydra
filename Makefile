# make link    use this checkout's build as `hydra` while developing
# make unlink  go back to the Homebrew build
LINK := $(HOME)/.local/bin/hydra

.PHONY: build test link unlink

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

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
