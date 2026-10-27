VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/joejenniges/fleet/internal/cli.Version=$(VERSION)

.PHONY: build test selfcheck e2e cross clean

build:
	go build -ldflags "$(LDFLAGS)" -o fleet ./cmd/fleet

test:
	go test ./...

selfcheck: build
	./fleet test selfcheck

# Live Claude Code scenarios (costs tokens). Override MODEL to use a small model.
e2e: build
	./fleet test standup
	./fleet test claude $(if $(MODEL),--model $(MODEL)) || (./fleet test teardown; exit 1)
	./fleet test teardown

cross:
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fleet-linux-amd64 ./cmd/fleet
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fleet-linux-arm64 ./cmd/fleet
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fleet-darwin-arm64 ./cmd/fleet
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fleet-darwin-amd64 ./cmd/fleet
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/fleet-windows-amd64.exe ./cmd/fleet

clean:
	rm -rf fleet dist
