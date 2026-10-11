.PHONY: test build vet render qualify-forgejo
test:
	go test -race ./...
vet:
	go vet ./...
build:
	CGO_ENABLED=0 go build -trimpath -o bin/agent-project-controller ./cmd/agent-project-controller
render:
	kubectl kustomize config/deploy
qualify-forgejo:
	python3 scripts/qualify-forgejo.py
