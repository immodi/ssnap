BINARY := ssnap
PKG    := ./cmd/ssnap

.PHONY: build install test race vet fmt clean

build:
	go build -o $(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY)
