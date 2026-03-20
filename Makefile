VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY := sage-router

.PHONY: build test lint release clean dashboard dev

build: dashboard
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/sage-router

dev:
	go run ./cmd/sage-router

test:
	go test ./... -race -cover -count=1

lint:
	golangci-lint run ./...

dashboard:
	@if [ -d "web/dashboard/node_modules" ]; then \
		cd web/dashboard && npm run build; \
	else \
		echo "Dashboard: using placeholder (run 'cd web/dashboard && npm install && npm run build' for full dashboard)"; \
	fi

release: dashboard
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 ./cmd/sage-router
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 ./cmd/sage-router
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64 ./cmd/sage-router
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 ./cmd/sage-router
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/sage-router
	cd dist && sha256sum * > checksums.txt 2>/dev/null || true

docker:
	docker build -t $(BINARY):$(VERSION) .

clean:
	rm -rf bin/ dist/
	rm -rf web/dashboard/dist/
