VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -ldflags "-s -w -X github.com/Zonos/m8/cmd.version=$(VERSION) -X github.com/Zonos/m8/cmd.commit=$(COMMIT) -X github.com/Zonos/m8/cmd.date=$(DATE)"

.PHONY: build install test lint clean

build:
	go build $(LDFLAGS) -o bin/m8 .

install:
	go install $(LDFLAGS) .

test:
	go test ./... -v

lint:
	golangci-lint run

clean:
	rm -rf bin/
