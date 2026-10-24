# Everyday commands. Plain go underneath, so nothing here is required.
#
#   make           build both binaries into bin/
#   make deploy    build into ../webmail-deploy/bin/, which the dev stack runs
#                  and restarts on change (docker-compose.dev.yml there)
#   make check     gofmt, go vet and go test, the gate before a commit

# Both database drivers are pure Go, so builds stay cgo-free (ADR 19), which
# also lets the binaries run on the Alpine containers of the dev stack.
export CGO_ENABLED = 0

DEPLOY_BIN ?= ../webmail-deploy/bin
DBML_TOOLS ?= dbml-tools

.PHONY: build deploy run test vet fmt check schema

build:
	go build -o bin/ ./cmd/...

deploy:
	go build -o $(DEPLOY_BIN)/ ./cmd/...

run:
	go run ./cmd/webmail-server

test:
	go test ./...

vet:
	go vet ./...

# Fails listing the files gofmt would change, rather than changing them.
fmt:
	@out=$$(gofmt -l cmd internal); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

check: fmt vet test

schema:
	schema/generate.sh $(DBML_TOOLS)
