GO ?= go
PREFIX ?= $(HOME)/.local

.PHONY: build test check model bench install verify-sync
build:
	cargo build --release --locked
	mkdir -p bin
	$(GO) build -trimpath -o bin/icarus ./cmd/icarus
	cp target/release/icarus-sync bin/icarus-sync

test:
	cargo test --locked
	$(GO) test -count=1 ./...

check:
	cargo fmt --check
	cargo clippy --locked --all-targets -- -D warnings
	$(GO) vet ./...

model:
	cargo build --locked
	sh verification/check.sh

bench: build
	cargo run --release --locked --example bench -- bin/icarus-sync

verify-sync:
	GO="$(GO)" sh verification/run.sh

install: build
	mkdir -p "$(DESTDIR)$(PREFIX)/bin"
	install -m 755 bin/icarus bin/icarus-sync "$(DESTDIR)$(PREFIX)/bin/"
