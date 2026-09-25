BIN := bin
CMDS := line-controller mr6c-bench mr6c-sim mercury230-bench mercury230-sim gen-serial-config

.PHONY: all lint test fuzz build pi sim bench meter-sim meter-bench meter-stand clean

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

# Fuzz декодера кадров «Меркурий 230».
fuzz:
	go test -run '^$$' -fuzz FuzzDecode -fuzztime 30s ./internal/hardware/mercury230

# «Меркурий 230»: симулятор за прозрачным мостом и стендовые проверки.
# make meter-bench ARGS="check"; ARGS="watch -interval 15s -duration 2h -csv run.csv"
meter-sim:
	go run ./cmd/mercury230-sim

ARGS ?= check
meter-bench:
	go run ./cmd/mercury230-bench -registry configs/bench.yaml $(ARGS)

# Адаптер против реального счётчика: MERCURY230_GATEWAY, MERCURY230_ADDRESS,
# MERCURY230_PASSWORD, [MERCURY230_SERIAL, MERCURY230_LEVEL, MERCURY230_BWRI].
meter-stand:
	go test -count=1 -v -run TestStand ./internal/hardware/mercury230

clean:
	rm -rf $(BIN)
