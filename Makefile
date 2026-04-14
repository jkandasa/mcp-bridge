BIN     := mcp-bridge
CMD     := ./cmd/mcp-bridge
CONFIG  := config.yaml

.PHONY: build run tidy vet fmt clean

build:
	go build -trimpath -ldflags="-s -w" -o $(BIN) $(CMD)

run: build
	./$(BIN) -config $(CONFIG)

tidy:
	go mod tidy

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BIN)
