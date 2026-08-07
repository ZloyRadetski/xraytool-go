BINARY     := xraytool
BUILD_DIR  := ./build
MAIN       := .

LDFLAGS := -s -w
GOFLAGS := CGO_ENABLED=0

.PHONY: build build-minimal build-linux clean install tidy

build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) $(MAIN)

build-minimal:
	go build -tags minimal -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-minimal $(MAIN)

build-linux:
	GOOS=linux GOARCH=amd64 $(GOFLAGS) go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-linux-amd64 $(MAIN)

install:
	go build -ldflags "$(LDFLAGS)" -o /usr/local/bin/$(BINARY) $(MAIN)

tidy:
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)
