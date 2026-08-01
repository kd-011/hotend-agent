package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Builds the verify-sig binary once and runs it as a real subprocess against
// generated keys — this tool's whole job is being trustworthy when invoked
// exactly the way install-agent.sh invokes it, so testing the CLI surface
// (args, exit codes) matters more than testing the library call directly.
func buildVerifySig(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "verify-sig")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifySig(t *testing.T) {
	bin := buildVerifySig(t)
	dir := t.TempDir()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("this is the agent binary bytes")
	sig := ed25519.Sign(priv, payload)

	payloadPath := writeFile(t, dir, "payload", payload)
	sigPath := writeFile(t, dir, "payload.sig", []byte(hex.EncodeToString(sig)))
	pubHex := hex.EncodeToString(pub)
	otherPubHex := hex.EncodeToString(otherPub)

	t.Run("valid signature, single trusted key", func(t *testing.T) {
		cmd := exec.Command(bin, payloadPath, sigPath, pubHex)
		if err := cmd.Run(); err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	t.Run("valid signature, key rotation list (old key first)", func(t *testing.T) {
		cmd := exec.Command(bin, payloadPath, sigPath, otherPubHex, pubHex)
		if err := cmd.Run(); err != nil {
			t.Fatalf("expected success matching the second key, got %v", err)
		}
	})

	t.Run("signature does not match any trusted key", func(t *testing.T) {
		cmd := exec.Command(bin, payloadPath, sigPath, otherPubHex)
		if err := cmd.Run(); err == nil {
			t.Fatal("expected failure, got success")
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		tamperedPath := writeFile(t, dir, "tampered", []byte("this is NOT the agent binary bytes"))
		cmd := exec.Command(bin, tamperedPath, sigPath, pubHex)
		if err := cmd.Run(); err == nil {
			t.Fatal("expected failure, got success")
		}
	})

	t.Run("missing signature file", func(t *testing.T) {
		cmd := exec.Command(bin, payloadPath, filepath.Join(dir, "nope.sig"), pubHex)
		if err := cmd.Run(); err == nil {
			t.Fatal("expected failure, got success")
		}
	})

	t.Run("malformed signature hex", func(t *testing.T) {
		badSigPath := writeFile(t, dir, "bad.sig", []byte("not hex at all"))
		cmd := exec.Command(bin, payloadPath, badSigPath, pubHex)
		if err := cmd.Run(); err == nil {
			t.Fatal("expected failure, got success")
		}
	})

	t.Run("missing args", func(t *testing.T) {
		cmd := exec.Command(bin, payloadPath, sigPath)
		if err := cmd.Run(); err == nil {
			t.Fatal("expected failure, got success")
		}
	})
}
