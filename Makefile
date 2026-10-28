.PHONY: all test build docker-build clean

IMAGE ?= ghcr.io/aobaiwaki123/inplace-gomemlimit-demo:latest

all: test build

test:
	go test -v -race ./...

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -extldflags '-static'" -trimpath -o bin/demo-app ./cmd/demo-app

docker-build:
	docker build -t $(IMAGE) .

clean:
	rm -rf bin/
