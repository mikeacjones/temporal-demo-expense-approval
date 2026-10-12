# Local development. Each target is one process; run them in separate
# terminals so you can stop the worker on its own during the demo.
#
#   make temporal   # local Temporal dev server (UI on http://localhost:8233)
#   make worker     # the Temporal worker
#   make web        # the app on http://localhost:8080
#
# Stop `make worker` with Ctrl-C at any point in the story, then start it again.

TEMPORAL_FLAGS := --disable-config-file

.PHONY: temporal worker web test build image

temporal:
	temporal server start-dev $(TEMPORAL_FLAGS) --ip 127.0.0.1

worker:
	go run ./cmd/worker

web:
	go run ./cmd/web

test:
	go test ./...

build:
	go build -o bin/ ./cmd/...

image:
	docker build -t expense-approval .
