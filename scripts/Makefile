.PHONY: build run test clean docker-up docker-down migrate

build:
	go build -o bin/papa cmd/crawler/main.go

run:
	go run cmd/crawler/main.go

test:
	go test -v -race -cover ./...

clean:
	rm -rf bin/ logs/*.log downloads/*

docker-up:
	docker-compose -f scripts/docker/docker-compose.yml up -d

docker-down:
	docker-compose -f scripts/docker/docker-compose.yml down

migrate:
	@echo "Running auto migration (set APP_ENV=dev to enable)"
	go run cmd/crawler/main.go