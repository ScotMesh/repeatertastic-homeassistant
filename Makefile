VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test bundle clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/repeatertastic-homeassistant ./cmd/repeatertastic-homeassistant

test:
	go vet ./...
	go test -race ./...

# The zip to install in RepeaterTastic (Plugins → Install plugin).
bundle:
	./scripts/bundle.sh $(VERSION)

clean:
	rm -rf bin dist
