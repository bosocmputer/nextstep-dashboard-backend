.PHONY: test vet verify context-sync context-verify context-test run worker migrate hash-password

test:
	go test ./...

vet:
	go vet ./...

verify: context-verify context-test test vet

context-sync:
	go run ./scripts/context-sync.go --write

context-verify:
	bash scripts/context-verify.sh
	go run ./scripts/context-sync.go --check

context-test:
	python3 scripts/context-impact-test.py
	go test ./scripts

run:
	go run ./cmd/api

worker:
	go run ./cmd/worker

migrate:
	go run ./cmd/migrate

hash-password:
	go run ./cmd/hash-password
