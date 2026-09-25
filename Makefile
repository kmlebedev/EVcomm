BIN := bin
CMDS := line-controller mr6c-bench mr6c-sim gen-serial-config

.PHONY: all lint test build pi sim bench clean

all: lint test build

# lint fails when anything is unformatted, then runs golangci-lint if it is
# installed. CI runs the same gate.
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not installed, skipping"; fi

test:
	go vet ./...
	go test -race ./...

build:
	@for c in $(CMDS); do go build -o $(BIN)/$$c ./cmd/$$c || exit 1; done

# Raspberry Pi 5, 64-разрядный Linux: статическая сборка без cgo.
pi:
	@for c in $(CMDS); do GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o $(BIN)/linux-arm64/$$c ./cmd/$$c || exit 1; done

sim:
	go run ./cmd/mr6c-sim

bench:
	go run ./cmd/mr6c-bench -registry configs/bench.yaml

clean:
	rm -rf $(BIN)
