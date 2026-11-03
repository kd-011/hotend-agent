// verify-sig: a tiny, dependency-free ed25519 signature checker.
//
// Used ONLY by the installer, and only on a FIRST install: the agent
// itself verifies every self-update with Go's own crypto/ed25519 (see
// signedByTrustedKey in ../main.go), but a shell script has no ed25519 of its
// own and the printer doesn't have the agent yet to borrow it from. The
// installer used to shell out to system openssl for this, which is
// inconsistent across printer images (missing entirely on some, too old to do
// one-shot Ed25519 (-rawin) on others) and silently skipped verification
// whenever it hit either case. This binary removes that dependency: it's
// static, a couple MB, and does exactly one thing, so "can we check the
// signature" no longer depends on what happens to be installed on the box.
//
// Usage: verify-sig <file> <sig-hex-file> <pubkey-hex> [pubkey-hex ...]
// Exit 0 = the file's signature verifies against one of the given keys.
// Exit 1 = anything else (bad args, unreadable input, no key matches).
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: verify-sig <file> <sig-hex-file> <pubkey-hex> [pubkey-hex ...]")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-sig:", err)
		os.Exit(1)
	}
	sigRaw, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-sig:", err)
		os.Exit(1)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(string(sigRaw)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		fmt.Fprintln(os.Stderr, "verify-sig: malformed signature")
		os.Exit(1)
	}
	for _, k := range os.Args[3:] {
		pub, err := hex.DecodeString(strings.TrimSpace(k))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue // not a well-formed key, not this agent's problem to explain
		}
		if ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
			os.Exit(0)
		}
	}
	fmt.Fprintln(os.Stderr, "verify-sig: signature does not verify against any given key")
	os.Exit(1)
}
