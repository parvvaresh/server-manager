BIN := bin/sshm-go
PREFIX ?= /usr/local

.PHONY: build install clean vet fmt

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/sshm

vet:
	go vet ./...

fmt:
	gofmt -w cmd/sshm

install: build
	install -m 755 $(BIN) $(PREFIX)/bin/sshm

clean:
	rm -f $(BIN)
