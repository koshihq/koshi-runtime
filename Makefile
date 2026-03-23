.PHONY: build test test-race cover lint docker docker-multiarch clean check-genops-spec validate-spec-version

BINARY := koshi
CMD := ./cmd/koshi

build:
	go build -o bin/$(BINARY) $(CMD)

test:
	go test ./...

test-race:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint:
	go vet ./...

docker:
	docker build -t $(BINARY):latest .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(BINARY):latest .

check-genops-spec:
	go test -run TestGenOpsSpec ./internal/genops/

validate-spec-version:
	./scripts/validate-spec-version.sh

clean:
	rm -rf bin/
