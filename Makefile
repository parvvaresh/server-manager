BIN := bin/sshm-go
PREFIX ?= /usr/local
VERSION ?= $(shell sed -n 's/^var version = "\(.*\)"/\1/p' cmd/sshm/main.go)
TARGETS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64

.PHONY: build install clean vet fmt dist npm

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) ./cmd/sshm

vet:
	go vet ./...

fmt:
	gofmt -w cmd/sshm

install: build
	install -m 755 $(BIN) $(PREFIX)/bin/sshm

# Cross compile every release target into dist/.
dist:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "build $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" \
			-o dist/sshm-$$os-$$arch ./cmd/sshm || exit 1; \
	done

# Stage the npm packages in dist/npm, as the release job does.
npm: dist
	node scripts/npm-stage.mjs $(VERSION) dist dist/npm

clean:
	rm -rf $(BIN) dist
