.PHONY: test vet verify run worker migrate hash-password

test:
	go test ./...

vet:
	go vet ./...

verify: test vet

run:
	go run ./cmd/api

worker:
	go run ./cmd/worker

migrate:
	go run ./cmd/migrate

hash-password:
	go run ./cmd/hash-password
