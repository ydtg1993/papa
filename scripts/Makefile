.PHONY: build test clean docker-up docker-down

build:
	go build -o bin/papa ./cmd/papa

test:
	go test -v -race -cover ./...

clean:
	rm -rf bin/ logs/*.log downloads/*

docker-up:
	docker-compose -f scripts/docker/docker-compose.yml up -d

docker-down:
	docker-compose -f scripts/docker/docker-compose.yml down
