.PHONY: build run test burst docker-up docker-down

build:
	go build -o server .
	go build -o burst ./cmd/burst

run: build
	./server

docker-up:
	docker compose up --build -d

docker-down:
	docker compose down -v

burst:
	./burst.sh $(BASE_URL)

test:
	go test ./...
