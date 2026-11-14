.PHONY: build test fmt vet clean

BIN := bin/runlet

build:
	go build -trimpath -o $(BIN) ./cmd/runlet

test:
	go test ./... -race -count=1

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf bin
