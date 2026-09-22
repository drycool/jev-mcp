BIN := jev-mcp

.PHONY: build test vet smoke clean all

all: build

# -buildvcs=false keeps the build working from a directory that is not a git
# checkout, which is how this gets built on a fresh field machine.
build:
	go build -buildvcs=false -o $(BIN) .

test:
	go test -count=1 ./...

vet:
	go vet ./...

# Drives the compiled binary over stdio against the live router. The Go tests use
# an HTTP stub, so only this proves the binary speaks the protocol to the real thing.
smoke: build
	python3 -u smoke_stdio.py

clean:
	rm -f $(BIN)
