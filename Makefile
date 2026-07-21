BIN := hotend-agent
DIST := dist
LDFLAGS := -s -w

.PHONY: build dist clean

# Build for the host.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

# Cross-compile the binaries a typical printer host needs.
dist:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64            go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-amd64 .
	GOOS=linux GOARCH=arm   GOARM=7    go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-arm .
	GOOS=linux GOARCH=arm64            go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-arm64 .

clean:
	rm -rf $(DIST) $(BIN)
