PORT ?= 8097

.PHONY: run test

run:
	PORT=$(PORT) go run .

test:
	go test ./...

docker:
	docker build -t iparent . && docker fun --rm \
		-p 8097:8097 \
		-v $(CURDIR)/config:/app/config \
		-v $(CURDIR)/data:/app/data \
		iparent
