# Go shopping-site sample — common dev commands.

.PHONY: build run test vet up down logs fmt seed

build:
	go build ./...

run:
	go run ./cmd/server

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Infra only (Postgres + Redis). Start the app separately with `make run`
# or bring up everything with `make infra-app`.
infra:
	docker compose up -d postgres redis

infra-app:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f