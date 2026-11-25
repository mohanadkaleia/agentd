PREFIX ?= $(HOME)/.local

.PHONY: build install test

# build writes bin/agentd.
build:
	go build -o bin/agentd ./cmd/agentd

# install links the build into $(PREFIX)/bin, so `agentd` works from any folder and a
# later `make build` updates it. Put $(PREFIX)/bin on your PATH.
install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/bin/agentd $(PREFIX)/bin/agentd
	@echo "Installed: $(PREFIX)/bin/agentd. Run: agentd"

test:
	go vet ./...
	go test ./...
