BINARY := fogd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X gitea.lan/matthew/fog/internal/config.Version=fogd-version=$(VERSION)

.PHONY: build frontend backend fog-node dev-backend dev-frontend test sdk-test conformance vet gen-types fmt typecheck deadcode check clean

build: frontend backend

backend:
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/fogd
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-setup ./cmd/fog-setup
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-locker ./cmd/fog-locker

frontend:
	cd frontend && npm ci
	ln -sfn ../../frontend/node_modules clients/chat-sdk/node_modules
	cd frontend && npm run build

# Cross-compile for the home server
build-linux: frontend
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 ./cmd/fogd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-arm64 ./cmd/fogd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-setup-linux-amd64 ./cmd/fog-setup
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-console-linux-amd64 ./cmd/fog-console
	# The locker reader: runs anywhere, reads a bucket back without a fog.
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-locker-linux-amd64 ./cmd/fog-locker
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-locker-darwin-arm64 ./cmd/fog-locker

# The compute node agent (docs/compute-contract.md), for every
# platform a machine lending fog its hardware may be. A development fogd
# serves these with FOG_NODE_BIN_DIR=bin; a release publishes them.
fog-node:
	for p in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do \
		CGO_ENABLED=0 GOOS=$${p%-*} GOARCH=$${p#*-} go build -trimpath -buildvcs=false -ldflags '$(LDFLAGS)' -o bin/fog-node-$$p ./cmd/fog-node || exit 1; \
	done

dev-backend:
	FOG_DATA_DIR=./.devdata FOG_DEV=1 FOG_NODE_BIN_DIR=bin go run ./cmd/fogd

dev-frontend:
	cd frontend && npm run dev

# Installer wizard against the mock agent: `make dev-setup` + `make dev-setup-frontend`.
# FOG_SETUP_REPAIR=1 gives the recovery menu; FOG_SETUP_MOCK_INSTALLED=1 the
# "this machine is already a Fog server" screen (stick left plugged in).
dev-setup:
	FOG_SETUP_MOCK=1 go run ./cmd/fog-setup

dev-setup-frontend:
	cd frontend && npm run dev:setup

# The flashable USB installer: builds the target OS and live environment in
# a Docker container, then assembles imaging/out/fog-installer-x86_64.img.
installer-image: build-linux
	imaging/build.sh all

# Boots the built image in QEMU, installs onto a virtual disk, reboots into
# it, and asserts the dashboard answers signed-in. The release gate for the
# image; slow without KVM.
installer-e2e:
	imaging/test/e2e.sh

test:
	go test -race -timeout 30m ./...

# The chat SDK has no package.json of its own: it borrows the frontend's
# node_modules through a symlink. Its tests need the link, and so does the
# frontend typecheck and build, which compile the SDK's source.
clients/chat-sdk/node_modules: frontend/node_modules
	ln -sfn ../../frontend/node_modules $@

# The chat SDK's own tests run under Node's type stripping (Node 22.6+).
sdk-test: clients/chat-sdk/node_modules
	cd clients/chat-sdk && node --test test/core.test.ts test/content-vectors.test.ts

# Run the chat conformance suite against a live fogd:
#   make conformance FOG_URL=http://fog.local:8080 FOG_OPERATOR_USER=admin FOG_OPERATOR_PASSWORD=…
conformance: clients/chat-sdk/node_modules
	cd clients/chat-sdk && FOG_URL=$(FOG_URL) FOG_OPERATOR_USER=$(FOG_OPERATOR_USER) FOG_OPERATOR_PASSWORD=$(FOG_OPERATOR_PASSWORD) FOG_OPERATOR_COOKIE=$(FOG_OPERATOR_COOKIE) node --experimental-strip-types --no-warnings test/conformance.mjs

vet:
	go vet ./...

# frontend/src/api.gen.ts, frontend/setup/types.gen.ts and
# clients/chat-sdk/src/wire.gen.ts are generated from the Go structs the APIs
# encode (outputs in internal/tools/tsgen/roots.go).
# `make test` (internal/tools/tsgen) fails while one is stale.
gen-types:
	go run ./internal/tools/tsgen

# gofmt -l lists what it would change and exits 0 either way, so the gate
# is on its output being empty.
fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; echo "not gofmt'd: run gofmt -w on the files above"; exit 1; fi

# npx tsc needs the toolchain installed; on a fresh clone this target
# bootstraps it so the release gate fails on real type errors, not on a
# missing node_modules.
frontend/node_modules: frontend/package-lock.json
	cd frontend && npm ci

# tsc, then eslint for the hooks rules (call order, dependency lists) tsc
# can't see.
typecheck: clients/chat-sdk/node_modules
	cd frontend && npm run lint

# Unreachable code fails the build: Go functions no binary or test reaches
# (scripts/deadcode.sh), and files, exports and dependencies nothing uses
# in the frontend and the chat SDK (knip).
deadcode: clients/chat-sdk/node_modules
	./scripts/deadcode.sh
	cd frontend && npm run knip

# Everything CI runs, in one command. release.sh gates on this, so a release
# cannot be cut from a tree that doesn't pass.
check: fmt vet test sdk-test typecheck deadcode

clean:
	rm -rf bin frontend/dist imaging/out
