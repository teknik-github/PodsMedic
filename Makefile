GO ?= go
IMAGE ?= ghcr.io/teknik-github/podsmedic:latest

.PHONY: build test vet fmt run docker deploy clean

build:
	$(GO) build -o bin/podsmedic ./cmd/podsmedic

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w ./cmd ./internal

# Runs against your current kubectl context, printing to stdout instead of
# posting to Slack or Telegram.
run:
	PODSMEDIC_DRY_RUN=true $(GO) run ./cmd/podsmedic

docker:
	docker build -t $(IMAGE) .

deploy:
	kubectl apply -f deploy/deployment.yaml
	kubectl apply -f deploy/rbac.yaml

clean:
	rm -rf bin
