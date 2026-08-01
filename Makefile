BIN := hotend-agent
DIST := dist
LDFLAGS := -s -w
# CGO_ENABLED=0 and -trimpath are not optional for a shipped binary:
#   static  — a dynamically linked build picks up the build machine's glibc and
#             refuses to start on an older printer image
#   trimpath— no local filesystem paths baked in, which is what makes the build
#             reproducible so anyone can compare their bytes to a published release
#   -buildvcs=false — no git-revision stamp, so a rebuild of this tree produces
#             byte-identical binaries to the published release regardless of
#             which clone (or mirror) it's built from
GO := CGO_ENABLED=0 go
BUILDFLAGS := -trimpath -buildvcs=false -ldflags "$(LDFLAGS)"

.PHONY: build dist test clean

# Build for the host.
build:
	$(GO) build $(BUILDFLAGS) -o $(BIN) .

# Cross-compile the binaries a typical printer host needs, plus the
# dependency-free signature checker the installer fetches (see verify-sig/).
dist:
	mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -o $(DIST)/$(BIN)-linux-amd64 .
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(BUILDFLAGS) -o $(DIST)/$(BIN)-linux-arm .
	GOOS=linux GOARCH=arm64 $(GO) build $(BUILDFLAGS) -o $(DIST)/$(BIN)-linux-arm64 .
	GOOS=linux GOARCH=amd64 $(GO) build $(BUILDFLAGS) -o $(DIST)/hotend-verify-sig-linux-amd64 ./verify-sig
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(BUILDFLAGS) -o $(DIST)/hotend-verify-sig-linux-arm ./verify-sig
	GOOS=linux GOARCH=arm64 $(GO) build $(BUILDFLAGS) -o $(DIST)/hotend-verify-sig-linux-arm64 ./verify-sig

test:
	$(GO) vet ./...
	$(GO) test ./...

clean:
	rm -rf $(DIST) $(BIN)
