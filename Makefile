.PHONY: build test lint docker-build docker-run clean

build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/anti-loop-proxy ./cmd/anti-loop-proxy

test:
	go test ./...

lint:
	go vet ./...

docker-build:
	docker build -t anti-loop-proxy:latest .

docker-run:
	docker run --rm -it -p 8080:8080 \
		-e ANTI_LOOP_UPSTREAM=https://api.openai.com \
		anti-loop-proxy:latest

clean:
	rm -rf bin
