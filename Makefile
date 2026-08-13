.PHONY: build test check

build:
	go build -o pi-monitor ./cmd/pi-monitor

test:
	go test ./...

check:
	gofmt -w $$(find . -name '*.go' -type f)
	go vet ./...
	go test ./...

