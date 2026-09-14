APP=app
.PHONY: build test fmt vet clean
build:
	CGO_ENABLED=0 go build -trimpath -o $(APP) ./cmd/app
test:
	go test ./...
fmt:
	gofmt -w cmd internal
vet:
	go vet ./...
clean:
	rm -f $(APP)
