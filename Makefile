.PHONY: build test vet install clean

BIN := tn
PKG := ./cmd/tn

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

install:
	go install -trimpath -ldflags "-s -w" $(PKG)

clean:
	rm -f $(BIN)
