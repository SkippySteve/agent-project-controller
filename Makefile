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
qualify-recovery:
	python3 scripts/qualify-recovery.py self-check

.PHONY: chart-check package
chart-check:
	APC_HELM_TEST=true go test -count=1 ./internal/installation
package: chart-check
	helm package charts/agent-project-controller --destination dist
