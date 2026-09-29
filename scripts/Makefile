.PHONY: build test clean

build:
	go build -o bin/papa ./cmd/papa

test:
	go test -v -race -cover ./...

clean:
	rm -rf bin/
