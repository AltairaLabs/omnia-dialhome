MODULE := github.com/AltairaLabs/omnia-dialhome
IMAGE  ?= omnia-dialhome:dev

.PHONY: all build test lint generate image

all: lint test

build:
	go build ./...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

# Regenerates runtimev1 from proto/omnia/runtime/v1/runtime.proto. Needs
# protoc, protoc-gen-go and protoc-gen-go-grpc on PATH.
generate:
	protoc -I proto \
		--go_out=. --go_opt=module=$(MODULE) \
		--go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
		omnia/runtime/v1/runtime.proto

image:
	docker build -t $(IMAGE) .
