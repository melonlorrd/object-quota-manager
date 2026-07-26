BINARY := object-quota-manager

.PHONY: all build test bench test-e2e generate clean

all: build

build:
	go build -o $(BINARY) .

test:
	go test ./...

bench:
	go test -bench=. -benchmem ./...

test-e2e:
	@./e2e.sh 2>/dev/null || true

generate:
	go generate ./...

clean:
	rm -f $(BINARY)
	go clean
