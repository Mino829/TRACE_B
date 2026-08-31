.PHONY: test integration vet build migrate cleanup
test:
	go test ./...
integration:
	@test -n "$$TRACE_TEST_DATABASE_URL" || (echo "Set TRACE_TEST_DATABASE_URL to a disposable PostgreSQL database"; exit 1)
	go test -race -count=1 ./...
vet:
	go vet ./...
build:
	go build -trimpath -o bin/trace-backend ./cmd/server
migrate:
	go run ./cmd/server migrate
cleanup:
	go run ./cmd/server cleanup
