package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Point the state file at a temp dir so the update-marker tests never touch a
// real agent's identity.
func tempState(t *testing.T) {
	t.Helper()
	orig := statePath
	statePath = filepath.Join(t.TempDir(), "agent.json")
	t.Cleanup(func() { statePath = orig })
}

func TestVersionNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2026.07.30.2", "2026.07.30.1", true},
		{"2026.07.30.1", "2026.07.30.2", false}, // downgrade
		{"2026.07.30.1", "2026.07.30.1", false}, // equal
		{"2026.07.30.10", "2026.07.30.9", true}, // numeric, not lexicographic
		{"2026.08.01.1", "2026.07.30.4", true},  // month rollover
		{"2027.01.01.1", "2026.12.31.9", true},  // year rollover
		{"2026.07.30", "2026.07.30.1", false},   // shorter = older
		{"2026.07.30.1", "2026.07.30", true},
	}
	for _, c := range cases {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Every trusted key must be a usable ed25519 public key, or updates fail closed
// on the whole fleet. Runs with no build artifacts, so CI always covers it.
func TestTrustedKeysAreValid(t *testing.T) {
	if len(updatePubKeysHex) == 0 {
		t.Fatal("no trusted release keys — self-update can never install anything")
	}
	seen := map[string]bool{}
	for _, h := range updatePubKeysHex {
		pub, err := hex.DecodeString(h)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			t.Errorf("trusted key %q is not a 32-byte hex ed25519 key", h)
		}
		if seen[h] {
			t.Errorf("trusted key %q is listed twice", h)
		}
		seen[h] = true
	}
}

func TestParseReleaseManifest(t *testing.T) {
	raw := []byte(`{
  "version": "2026.07.30.5",
  "files": {
    "amd64": { "sha256": "abc", "size": 10 },
    "arm64": { "sha256": "def", "size": 11 },
    "arm": { "sha256": "ghi", "size": 12 }
  }
}`)
	m, err := parseReleaseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "2026.07.30.5" {
		t.Fatalf("version = %q", m.Version)
	}
	if m.Files["amd64"].SHA256 != "abc" || m.Files["amd64"].Size != 10 {
		t.Fatalf("amd64 entry = %+v", m.Files["amd64"])
	}
	if _, err := parseReleaseManifest([]byte(`{"version":"x"}`)); err == nil {
		t.Fatal("expected error for missing files")
	}
}

func TestSHA256Hex(t *testing.T) {
	got := sha256Hex([]byte("hotend"))
	if len(got) != 64 {
		t.Fatalf("sha256 hex length = %d", len(got))
	}
	if sha256Hex([]byte("hotend")) != got {
		t.Fatal("sha256Hex not stable")
	}
	if sha256Hex([]byte("hotend")) == sha256Hex([]byte("hotend!")) {
		t.Fatal("different inputs produced same hash")
	}
}

// Release signatures must verify against a key
// baked into the agent. This is the interop check between Node's signer and Go's
// verifier, run over the real release artifacts when they exist.
func TestReleaseSignaturesVerify(t *testing.T) {
	checked := 0
	for _, arch := range []string{"amd64", "arm64", "arm"} {
		bin := "hotend-agent-linux-" + arch
		data, err := os.ReadFile(bin)
		if err != nil {
			continue // not built in this checkout, nothing to verify
		}
		sb, err := os.ReadFile(bin + ".sig")
		if err != nil {
			// An unsigned build is normal in CI (no release key there). The signed
			// -release gate is the publish script, which refuses to publish a
			// binary without a fresh .sig.
			continue
		}
		sig, err := hex.DecodeString(strings.TrimSpace(string(sb)))
		if err != nil || len(sig) != ed25519.SignatureSize {
			t.Errorf("%s.sig is not a hex ed25519 signature", bin)
			continue
		}
		if !signedByTrustedKey(data, sig) {
			t.Errorf("%s: signature does NOT verify against any trusted key — key mismatch or stale .sig", bin)
			continue
		}
		// Tampering must break the signature.
		data[len(data)/2] ^= 0xff
		if signedByTrustedKey(data, sig) {
			t.Errorf("%s: tampered binary still verifies — verification is broken", bin)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no signed binaries to verify")
	}
}

// The advertised-version check must agree with what -version prints, or every
// update gets rejected by the run-and-report guard.
func TestVersionFlagMatchesConstant(t *testing.T) {
	if _, err := os.Stat("hotend-agent-linux-amd64"); err != nil {
		t.Skip("amd64 binary not built")
	}
	out, err := exec.Command("./hotend-agent-linux-amd64", "-version").Output()
	if err != nil {
		t.Skipf("cannot exec amd64 binary here: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != agentVersion {
		t.Errorf("-version prints %q but agentVersion is %q — rebuild before signing", got, agentVersion)
	}
}

// A binary on probation gets updateProveBoots chances to reach the relay, then
// the release is written off. This is the guard that keeps a bad release from
// needing a physical visit to every printer.
func TestPendingBootVerdict(t *testing.T) {
	tempState(t)
	mutateState(func(s *agentState) {
		s.PrinterID = "abc123"
		s.PendingVersion = agentVersion
		s.PendingSince = time.Now().UTC().Format(time.RFC3339)
	})
	for i := 1; i <= updateProveBoots; i++ {
		if why := pendingBootVerdict(); why != "" {
			t.Fatalf("boot %d: reverted too early (%s)", i, why)
		}
	}
	if why := pendingBootVerdict(); why == "" {
		t.Fatalf("boot %d: should have given up on this release", updateProveBoots+1)
	}
	if readState().PrinterID != "abc123" {
		t.Error("the probation bookkeeping clobbered the printer identity")
	}
}

// A marker naming some OTHER version means we are not the binary it describes
// (a revert already happened, or the swap never took), so it must be discarded,
// not counted against the running binary.
func TestPendingMarkerForAnotherVersionIsDropped(t *testing.T) {
	tempState(t)
	mutateState(func(s *agentState) { s.PendingVersion = "1999.01.01.1"; s.PendingBoots = 99 })
	if why := pendingBootVerdict(); why != "" {
		t.Fatalf("stale marker triggered a revert: %s", why)
	}
	if got := readState().PendingVersion; got != "" {
		t.Errorf("stale marker still present: %q", got)
	}
}

// A successful connect clears probation, so the next boot starts clean.
func TestConnectClearsProbation(t *testing.T) {
	tempState(t)
	mutateState(func(s *agentState) { s.PendingVersion = agentVersion; s.PendingBoots = 2 })
	mutateState(func(s *agentState) {
		if s.PendingVersion == agentVersion {
			clearPending(s)
		}
	})
	st := readState()
	if st.PendingVersion != "" || st.PendingBoots != 0 {
		t.Errorf("probation not cleared: %+v", st)
	}
}

// Rolling back restores the previous binary and remembers the version it escaped
// or the agent downloads the same broken release again in 6 hours.
func TestRevertRestoresPreviousBinaryAndBlacklists(t *testing.T) {
	tempState(t)
	dir := t.TempDir()
	self := filepath.Join(dir, "hotend-agent")
	if err := os.WriteFile(self, []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(self+prevSuffix, []byte("known-good"), 0o755); err != nil {
		t.Fatal(err)
	}
	mutateState(func(s *agentState) { s.PendingVersion = agentVersion; s.PendingBoots = 4 })

	if !revertTo(self, "test") {
		t.Fatal("revertTo should report that the caller must exit")
	}
	if b, _ := os.ReadFile(self); string(b) != "known-good" {
		t.Errorf("previous binary not restored, got %q", b)
	}
	if _, err := os.Stat(self + prevSuffix); !os.IsNotExist(err) {
		t.Error("the .prev copy should have been consumed by the rename")
	}
	st := readState()
	if st.PendingVersion != "" {
		t.Error("probation should be cleared after a revert")
	}
	if len(st.BadVersions) != 1 || st.BadVersions[0] != agentVersion {
		t.Errorf("BadVersions = %v, want [%s]", st.BadVersions, agentVersion)
	}
}

// With nothing to roll back to, reverting must be a no-op that clears the marker
// rather than a boot loop.
func TestRevertWithoutPreviousBinary(t *testing.T) {
	tempState(t)
	self := filepath.Join(t.TempDir(), "hotend-agent")
	if err := os.WriteFile(self, []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	mutateState(func(s *agentState) { s.PendingVersion = agentVersion; s.PendingBoots = 9 })
	if revertTo(self, "test") {
		t.Error("revertTo should not ask the caller to exit when there is no .prev")
	}
	if readState().PendingVersion != "" {
		t.Error("marker should be cleared so the check doesn't repeat every boot")
	}
}

func TestJitterStaysInRange(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
	for i := 0; i < 200; i++ {
		if got := jitter(time.Hour); got < 0 || got >= time.Hour {
			t.Fatalf("jitter(1h) = %v, out of range", got)
		}
	}
}

// Regression. The proxied path arrives
// percent-encoded; assigning it to url.URL.Path (the DECODED field) made
// String() escape the '%' again, so "%20" went out as "%2520" and Moonraker
// 404'd every file whose name contains a space or '+', roughly half a real
// g-code library, plus the matching timelapses and thumbnails.
func TestBuildLocalURLPreservesEncoding(t *testing.T) {
	const base = "http://127.0.0.1:7125"
	cases := []struct {
		name, in, want string
	}{
		{"space", "/server/files/gcodes/My%20Print.gcode", base + "/server/files/gcodes/My%20Print.gcode"},
		{"plus", "/server/files/timelapse/153-00-02%2B-%2BDietro.mp4", base + "/server/files/timelapse/153-00-02%2B-%2BDietro.mp4"},
		{"literal plus", "/server/files/gcodes/Perfect+clip.gcode", base + "/server/files/gcodes/Perfect+clip.gcode"},
		{"nested dirs", "/server/files/gcodes/Cable%20Management/Clip.gcode", base + "/server/files/gcodes/Cable%20Management/Clip.gcode"},
		{"plain", "/server/files/gcodes/plain_name.gcode", base + "/server/files/gcodes/plain_name.gcode"},
		{"query kept", "/server/files/list?root=gcodes", base + "/server/files/list?root=gcodes"},
		{"encoded path with query", "/server/files/metadata?filename=My%20Print.gcode", base + "/server/files/metadata?filename=My%20Print.gcode"},
	}
	for _, c := range cases {
		got, err := buildLocalURL(base, c.in)
		if err != nil {
			t.Errorf("%s: buildLocalURL(%q) errored: %v", c.name, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: buildLocalURL(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// Paths that could escape the host or aren't a path at all stay rejected.
	for _, bad := range []string{"relative/path", "//evil.example.com/x", "/\\evil"} {
		if _, err := buildLocalURL(base, bad); err == nil {
			t.Errorf("buildLocalURL(%q) should have been rejected", bad)
		}
	}
}

// Regression. The installer adds
// [spoolman] to moonraker.conf, which makes the agent stand down from its own
// print_stats counting so a print isn't deducted twice. But Moonraker only
// deducts from the spool IT has selected, and nothing ever set that: choosing a
// spool in the app is intercepted by handleMoonrakerSpoolman and written to the
// central store, which Moonraker never sees. Its spool_id stayed null, so those
// printers recorded NO filament usage at all while the app showed a healthy
// active spool ("1 kg left" after a finished print).
func TestNeedsSpoolSync(t *testing.T) {
	id := func(n int) *int { return &n }
	cases := []struct {
		name            string
		moonraker, want *int
		expect          bool
	}{
		{"moonraker unset, hotend has one — the bug", nil, id(7), true},
		{"different spools — app switched", id(3), id(7), true},
		{"already in step — no write", id(7), id(7), false},
		{"nothing selected in hotend — leave the printer alone", id(3), nil, false},
		{"neither side has one", nil, nil, false},
	}
	for _, c := range cases {
		if got := needsSpoolSync(c.moonraker, c.want); got != c.expect {
			t.Errorf("%s: needsSpoolSync() = %v, want %v", c.name, got, c.expect)
		}
	}
}

// resolvePrivateHost must hand the caller the IP it validated, not just a
// yes/no. A caller that re-resolves the hostname to dial (as doTCPOpen used
// to) reopens the DNS-rebinding hole this check exists to close: a relay
// pointing at a rebinding domain could pass the check with a private answer
// and get a public one on the second lookup.
func TestResolvePrivateHostRejectsPublicTargets(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"loopback literal", "127.0.0.1", true},
		{"private literal", "192.168.1.50", true},
		{"link-local literal", "169.254.1.1", true},
		{"public literal", "8.8.8.8", false},
		{"unresolvable", "this-host-does-not-exist.invalid", false},
	}
	for _, c := range cases {
		ip, ok := resolvePrivateHost(c.host)
		if ok != c.want {
			t.Errorf("%s: resolvePrivateHost(%q) ok = %v, want %v", c.name, c.host, ok, c.want)
		}
		if ok && ip == nil {
			t.Errorf("%s: resolvePrivateHost(%q) returned ok=true with nil IP", c.name, c.host)
		}
	}
}
