.PHONY: build test vet clean

BINARY := gmlc
VERSION := 0.1.1d

build:
	mkdir -p bin
	go build -ldflags "-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/gmlc

test:
	go test ./... -count=1

vet:
	go vet ./...

clean:
	rm -rf bin

FUZZ_TIME ?= 2s
fuzz-smoke:
	go test ./internal/gad -run=^$$ -fuzz=FuzzDecode -fuzztime=$(FUZZ_TIME)
	go test ./internal/slh -run=^$$ -fuzz=FuzzEncodeMSISDN -fuzztime=$(FUZZ_TIME)
	go test ./internal/slg -run=^$$ -fuzz=FuzzDecodePLA -fuzztime=$(FUZZ_TIME)
	go test ./internal/slg -run=^$$ -fuzz=FuzzDecodeLRR -fuzztime=$(FUZZ_TIME)
	go test ./internal/httpapi -run=^$$ -fuzz=FuzzLocationRequestJSON -fuzztime=$(FUZZ_TIME)
