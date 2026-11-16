BINARY  := mikrotik-presence-sensor
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race vet test-broker docker run check-config clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

# Integration test against a real broker, e.g. make test-broker BROKER=tcp://[::1]:1883
test-broker:
	MPS_TEST_BROKER=$(BROKER) go test -count=1 -run Broker -v ./internal/mqtt

docker:
	docker build --build-arg VERSION=$(VERSION) -t ghcr.io/endreszabo/$(BINARY):$(VERSION) .

check-config: build
	./$(BINARY) -check-config -config $(or $(CONFIG),config.yaml)

run: build
	./$(BINARY) -config $(or $(CONFIG),config.yaml)

clean:
	rm -f $(BINARY)
