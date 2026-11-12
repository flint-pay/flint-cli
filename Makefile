.PHONY: build test contract coverage

build:
	go build -trimpath -buildvcs=false -o bin/flint ./cmd/flint

test:
	npm ci --prefix e2e/receiver --ignore-scripts --no-audit --no-fund
	go test -race ./...

contract: test
	@report=$$(mktemp); trap 'rm -f "$$report"' EXIT; \
	go run ./cmd/flint-coverage > "$$report" && diff -u coverage.json "$$report"

coverage:
	go run ./cmd/flint-coverage > coverage.json
