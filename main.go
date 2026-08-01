// Hotend printer agent — single static binary, no runtime deps. Dials OUT to the
// relay, then bridges whatever the relay sends to the local Moonraker (+ webcam).
// A faithful port of scripts/relay-agent.mjs (same framed protocol).
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Bump on every agent release. Agents auto-update to whatever /agent-bin/version
// advertises (see selfUpdate). Date-based so "newer" is obvious.
const agentVersion = "2026.08.01.2"

// Updates must be SIGNED. The relay serves <binary>.sig — hex ed25519 over the
// exact binary bytes — and selfUpdate refuses anything that doesn't verify
// against one of these baked-in public keys. The private key lives offline with
// the maintainer (scripts/sign-agent-release.mjs), NOT on the droplet: a
// compromised relay can advertise whatever version it likes, but it cannot mint
// a signature, so nothing unsigned ever installs.
//
// A LIST, not a constant, so the key can be ROTATED without stranding the fleet:
// ship a release that trusts both the old and the new key, wait for agents to
// adopt it, then publish one signed by the new key and drop the old entry. With a
// single key there is no path off it — a lost key means no more updates, ever.
var updatePubKeysHex = []string{
	// Primary — signs every routine release (~/.config/hotend/agent-signing.pem).
	"6ebe81a90a883f61d6ca9c1947de6fce4b591c895f1c8cc7812e08311394af68",
	// Disaster-recovery backup — private half lives OFFLINE, physically separate
	// from the primary. Only ever used to sign the release that rotates off a
	// lost or compromised primary. Losing one key no longer strands the fleet.
	"fa752823ce4cdeea2fda15a7ac297b1595d63fdc52a8f3f37fa5ab31ef5d0525",
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

var (
	relayURL  = strings.TrimRight(env("HOTEND_RELAY_URL", ""), "/")
	localURL  = strings.TrimRight(env("HOTEND_LOCAL_URL", "http://127.0.0.1:7125"), "/")
	webcamURL = strings.TrimRight(env("HOTEND_WEBCAM_URL", "http://127.0.0.1:8080"), "/")
	claim     = os.Getenv("HOTEND_CLAIM")
	// Identity lives in a DURABLE, install-dir-independent location so a reinstall
	// (which typically wipes ~/.hotend-agent) never loses the printerId + device
	// secret — losing them orphans the printer and forces a confusing re-pair.
	statePath = env("HOTEND_AGENT_STATE", defaultStatePath())
)

func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "hotend")
	}
	return filepath.Join(home(), ".config", "hotend")
}

func defaultStatePath() string { return filepath.Join(configDir(), "agent.json") }

// Legacy identity locations to migrate FROM (pre-2026.07.22 installs kept state
// inside the deletable install dir, or in a dotfile). Read-only; never written.
func legacyStatePaths() []string {
	return []string{
		filepath.Join(home(), ".hotend-agent", "agent.json"),
		filepath.Join(home(), ".hotend-agent.json"),
	}
}

// Headers Cloudflare/relays add that make Moonraker treat the (localhost) client
// as an untrusted proxied IP → 401 / rejected WS. Strip them so it reads as a
// direct local call. Also strip Accept-Encoding so we always get plain bytes.
var stripReq = map[string]bool{
	"x-forwarded-for": true, "x-forwarded-proto": true, "x-forwarded-host": true,
	"x-real-ip": true, "cf-connecting-ip": true, "cf-ray": true, "cf-ipcountry": true,
	"cf-visitor": true, "forwarded": true, "authorization": true, "origin": true,
	"accept-encoding": true, "host": true, "connection": true, "keep-alive": true,
}

type msg struct {
	T            string            `json:"t"`
	ID           string            `json:"id,omitempty"`
	Method       string            `json:"method,omitempty"`
	Path         string            `json:"path,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	B64          string            `json:"b64,omitempty"`
	Binary       bool              `json:"binary,omitempty"`
	Target       string            `json:"target,omitempty"`
	Status       int               `json:"status,omitempty"`
	Code         string            `json:"code,omitempty"`
	ExpiresInMin int               `json:"expiresInMin,omitempty"`
	Account      string            `json:"account,omitempty"`
	Secret       string            `json:"secret,omitempty"`     // device secret issued by the relay
	WebcamURL    string            `json:"webcamUrl,omitempty"`  // per-printer webcam URL override
	WebcamType   string            `json:"webcamType,omitempty"` // 'mjpeg' | 'hls' (auto-detected)
	Body         json.RawMessage   `json:"body,omitempty"`       // spoolman request/response body
}

// The current live relay connection, so the long-lived local Spoolman HTTP
// server can forward over whichever connection is up (survives reconnects).
var (
	curConn      atomic.Pointer[conn]
	spoolmanPort = env("HOTEND_SPOOLMAN_PORT", "7913")
	smSeq        uint64
)

// Persistent agent identity: the printerId plus the device secret the relay
// issues on first connect. The secret is presented (in a header, never the URL)
// on every subsequent connect so a bare printerId can't be replayed to hijack
// the printer.
var (
	stateMu sync.Mutex
	// Atomic because the self-updater goroutine now starts before main has loaded
	// the state file, so it may read this while main is still writing it.
	agentPrinter  atomic.Pointer[string]
	agentSecretMu sync.RWMutex
	agentSecret   string
	// Per-printer webcam URL override, set live by the relay via a 'config' message
	// (or on connect). Empty → fall back to detected, then the install-time default.
	webcamMu       sync.RWMutex
	webcamOverride string
	// Auto-detected from Moonraker's webcam registry at startup (see detectWebcam).
	detectedMu         sync.RWMutex
	detectedWebcamURL  string
	detectedWebcamType string // "mjpeg" | "hls"
)

// effectiveWebcam is the URL the agent proxies the camera to: the relay-set manual
// override wins; else the URL auto-detected from Moonraker; else the installed default.
func effectiveWebcam() string {
	webcamMu.RLock()
	o := webcamOverride
	webcamMu.RUnlock()
	if o != "" {
		return o
	}
	detectedMu.RLock()
	d := detectedWebcamURL
	detectedMu.RUnlock()
	if d != "" {
		return d
	}
	return webcamURL
}

func setPrinterID(id string) { agentPrinter.Store(&id) }

func printerID() string {
	if p := agentPrinter.Load(); p != nil {
		return *p
	}
	return ""
}

type agentState struct {
	PrinterID    string `json:"printerId"`
	DeviceSecret string `json:"deviceSecret,omitempty"`
	// Self-update probation. Set the instant a new binary is swapped in, cleared
	// the instant that binary reaches the relay. See checkPendingUpdate.
	PendingVersion string `json:"pendingVersion,omitempty"`
	PendingSince   string `json:"pendingSince,omitempty"` // RFC3339
	PendingBoots   int    `json:"pendingBoots,omitempty"`
	// Versions that were installed and could not connect. Never installed again —
	// otherwise a reverted agent just re-downloads the broken release it escaped.
	BadVersions []string `json:"badVersions,omitempty"`
}

func loadState() agentState {
	var s agentState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &s)
	}
	// No identity at the durable path — before minting a brand-new one (which would
	// orphan an already-linked printer), try to migrate from a legacy location. A
	// reinstall that moved/removed the old install dir keeps the SAME printer.
	if s.PrinterID == "" {
		for _, p := range legacyStatePaths() {
			if p == statePath {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var old agentState
			if json.Unmarshal(b, &old) == nil && old.PrinterID != "" {
				s = old
				saveState(s)
				fmt.Println("[agent] migrated printer identity from", p)
				return s
			}
		}
	}
	if s.PrinterID == "" {
		raw := make([]byte, 16)
		rand.Read(raw)
		s.PrinterID = hex.EncodeToString(raw)
		saveState(s)
	}
	return s
}

func saveState(s agentState) {
	stateMu.Lock()
	defer stateMu.Unlock()
	os.MkdirAll(filepath.Dir(statePath), 0o700)
	b, _ := json.Marshal(s)
	os.WriteFile(statePath, b, 0o600)
}

// mutateState read-modify-writes the state file. Use this rather than building an
// agentState and calling saveState: a from-scratch struct silently drops every
// field the caller didn't think about (the update markers below, for instance).
func mutateState(fn func(*agentState)) {
	stateMu.Lock()
	defer stateMu.Unlock()
	var s agentState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &s)
	}
	fn(&s)
	os.MkdirAll(filepath.Dir(statePath), 0o700)
	if b, err := json.Marshal(s); err == nil {
		os.WriteFile(statePath, b, 0o600)
	}
}

func readState() agentState {
	var s agentState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(agentVersion)
		return
	}
	// Everything below this point can fail on a bad release. These two lines run
	// FIRST, before any check that could exit, because they are what makes a bad
	// release survivable: the probation check reverts a binary that can't work,
	// and the updater keeps polling even if the rest of startup refuses to run —
	// so a fix can always be published instead of visiting every printer.
	checkPendingUpdate()
	go updateLoop()
	if relayURL == "" {
		fmt.Fprintln(os.Stderr, "[agent] set HOTEND_RELAY_URL")
		os.Exit(1)
	}
	if !strings.HasPrefix(relayURL, "wss://") &&
		!strings.HasPrefix(relayURL, "ws://127.0.0.1") && !strings.HasPrefix(relayURL, "ws://localhost") {
		fmt.Fprintf(os.Stderr, "[agent] refusing insecure relay URL (%s) — must be wss://\n", relayURL)
		os.Exit(1)
	}
	st := loadState()
	id := st.PrinterID
	setPrinterID(id)
	agentSecret = st.DeviceSecret
	go watchPendingUpdate()    // revert if this binary never reaches the relay
	go serveSpoolman()         // local Spoolman endpoint (Option A: Moonraker [spoolman] → store)
	go startFilamentTracking() // Option B usage tracking ONLY if Moonraker has no [spoolman]
	go func() {                // auto-detect the webcam from Moonraker's registry (retry while it boots)
		for i := 0; i < 30; i++ {
			if u, t := detectWebcam(); u != "" {
				detectedMu.Lock()
				detectedWebcamURL, detectedWebcamType = u, t
				detectedMu.Unlock()
				fmt.Println("[agent] detected webcam:", t, u)
				if cc := curConn.Load(); cc != nil {
					reportDetectedWebcam(cc)
				}
				return
			}
			time.Sleep(10 * time.Second)
		}
	}()
	fmt.Println("[agent] hotend-agent", agentVersion, "starting")
	backoff := time.Second
	for {
		if err := connect(id); err != nil {
			fmt.Println("[agent] disconnected; reconnecting:", err)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// How long a freshly installed binary has to reach the relay once, and how many
// boots it gets, before it is written off as a bad release and rolled back.
const (
	prevSuffix       = ".prev"
	updateProveAfter = 15 * time.Minute
	updateProveBoots = 3
)

// Set on the first successful relay connection — the proof that the running
// binary actually works.
var connectedOnce atomic.Bool

func clearPending(s *agentState) {
	s.PendingVersion, s.PendingSince, s.PendingBoots = "", "", 0
}

// updateLoop checks for a new release on boot and then every ~6h. The interval is
// jittered so a site-wide power cut doesn't make the whole fleet poll (and then
// download several MB) at the same instant.
func updateLoop() {
	// Give main a moment to load the printer identity, so the relay can honour the
	// account's auto-update opt-out. If startup never gets that far we poll anyway
	// — an agent that can't start is exactly the one that needs a new binary.
	for i := 0; i < 30 && printerID() == ""; i++ {
		time.Sleep(time.Second)
	}
	for {
		selfUpdate()
		time.Sleep(6*time.Hour + jitter(time.Hour))
	}
}

// jitter returns a random duration in [0, max).
func jitter(max time.Duration) time.Duration {
	var b [4]byte
	if max <= 0 {
		return 0
	}
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return time.Duration(uint64(binary.BigEndian.Uint32(b[:])) % uint64(max))
}

// checkPendingUpdate is the rollback half of self-update, and it runs before
// anything else in main. A binary that was just installed is on probation: if it
// burns updateProveBoots boots without ever reaching the relay, the previous
// binary goes back in and the version is blacklisted. Without this, a release
// that dies during startup is unrecoverable remotely — the old binary is gone,
// the downgrade guard refuses anything older, and every printer needs a visit.
func checkPendingUpdate() {
	defer func() { _ = recover() }()
	// Only write a release off when the relay is actually reachable. A printer
	// whose internet died right after updating would otherwise blacklist a
	// perfectly good version — the boot counter stays over the line, so the revert
	// simply happens on the first boot that has a network.
	if why := pendingBootVerdict(); why != "" && relayReachable() {
		revertUpdate(why)
	}
}

// pendingBootVerdict advances the boot counter of a binary on probation and
// returns a non-empty reason when the release has run out of chances.
func pendingBootVerdict() string {
	st := readState()
	if st.PendingVersion == "" {
		return ""
	}
	if st.PendingVersion != agentVersion {
		mutateState(clearPending) // stale marker: we are not the binary it describes
		return ""
	}
	boots := st.PendingBoots + 1
	mutateState(func(s *agentState) { s.PendingBoots = boots })
	if boots > updateProveBoots {
		return fmt.Sprintf("no relay connection in %d boots", boots)
	}
	return ""
}

// watchPendingUpdate covers the other failure shape: a new binary that starts
// cleanly but can never connect (bad relay URL handling, broken TLS, a protocol
// change the relay doesn't speak). Just as fatal as a crash, so it gets the same
// rollback on a timer.
func watchPendingUpdate() {
	defer func() { _ = recover() }()
	if readState().PendingVersion != agentVersion {
		return
	}
	// Keep waiting while the printer simply has no internet — that says nothing
	// about the binary. Only a relay we can reach over HTTP, and still cannot open
	// a WebSocket to, condemns the release.
	for i := 0; i < 8; i++ {
		time.Sleep(updateProveAfter)
		if connectedOnce.Load() {
			return
		}
		if relayReachable() {
			revertUpdate(fmt.Sprintf("relay reachable but no connection within %s of install", updateProveAfter))
			return
		}
	}
}

// relayReachable reports whether the relay answers HTTP at all — the discriminator
// between "this binary is broken" and "this printer is offline".
func relayReachable() bool {
	if relayURL == "" {
		return false
	}
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Get(relayHTTPBase() + "/agent-bin/version")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// The relay's HTTP origin, derived from the WebSocket URL it was configured with.
func relayHTTPBase() string {
	return strings.Replace(strings.Replace(relayURL, "wss://", "https://", 1), "ws://", "http://", 1)
}

// revertUpdate puts the previous binary back and restarts into it. The version
// being abandoned is remembered, because the relay is still advertising it and
// selfUpdate would otherwise download it straight back.
func revertUpdate(why string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	if revertTo(self, why) {
		restartSelf()
	}
}

// revertTo restores <self>.prev over self and blacklists the running version.
// Returns true when the caller should restart into the restored binary.
func revertTo(self, why string) bool {
	prev := self + prevSuffix
	if _, err := os.Stat(prev); err != nil {
		// Nothing to go back to (a fresh install, or the copy failed). Clear the
		// marker so this check doesn't re-run on every boot forever.
		fmt.Printf("[agent] %s looks broken (%s) but there is no previous binary to restore\n", agentVersion, why)
		mutateState(clearPending)
		return false
	}
	bad := agentVersion
	mutateState(func(s *agentState) {
		clearPending(s)
		for _, v := range s.BadVersions {
			if v == bad {
				return
			}
		}
		s.BadVersions = append(s.BadVersions, bad)
		if len(s.BadVersions) > 10 {
			s.BadVersions = s.BadVersions[len(s.BadVersions)-10:]
		}
	})
	if err := os.Rename(prev, self); err != nil {
		fmt.Println("[agent] revert failed:", err)
		return false
	}
	fmt.Printf("[agent] %s is bad (%s) — restored the previous binary; restarting\n", bad, why)
	return true
}

// versionNewer reports whether a is strictly newer than b. Versions are dotted
// numbers ("2026.07.30.2"); numeric per-part comparison, so ".10" beats ".9".
// Refusing anything not strictly newer is the downgrade guard: a compromised
// relay can't roll agents back to an older (signed, but vulnerable) release.
func versionNewer(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bn, _ = strconv.Atoi(bs[i])
		}
		if an != bn {
			return an > bn
		}
	}
	return false
}

// releaseManifest is the signed statement that binds an advertised version to
// the exact bytes of each architecture's binary. Served as /agent-bin/manifest
// (+ .sig). Signing the manifest (not just each binary) means a compromised
// relay cannot pair yesterday's signed amd64 with today's version string.
type releaseManifest struct {
	Version string `json:"version"`
	Files   map[string]struct {
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"files"`
}

func parseReleaseManifest(raw []byte) (releaseManifest, error) {
	var m releaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.Version == "" || len(m.Files) == 0 {
		return m, fmt.Errorf("manifest missing version or files")
	}
	return m, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fetchReleaseManifest downloads and verifies the signed release manifest.
// ok=false with a nil error means "not published" (404) — callers may fall back
// for one transition release. Any other failure is fail-closed (non-nil error).
func fetchReleaseManifest(cl *http.Client, httpBase, wantVersion string) (releaseManifest, bool, error) {
	r, err := cl.Get(httpBase + "/agent-bin/manifest")
	if err != nil {
		return releaseManifest{}, false, err
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusNotFound {
		return releaseManifest{}, false, nil
	}
	if r.StatusCode != 200 {
		return releaseManifest{}, false, fmt.Errorf("manifest HTTP %d", r.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || len(raw) == 0 {
		return releaseManifest{}, false, fmt.Errorf("empty manifest")
	}
	rs, err := cl.Get(httpBase + "/agent-bin/manifest.sig")
	if err != nil {
		return releaseManifest{}, false, err
	}
	defer rs.Body.Close()
	if rs.StatusCode != 200 {
		return releaseManifest{}, false, fmt.Errorf("manifest.sig HTTP %d", rs.StatusCode)
	}
	sb, _ := io.ReadAll(io.LimitReader(rs.Body, 1024))
	sig, err := hex.DecodeString(strings.TrimSpace(string(sb)))
	if err != nil || len(sig) != ed25519.SignatureSize || !signedByTrustedKey(raw, sig) {
		return releaseManifest{}, false, fmt.Errorf("manifest signature invalid")
	}
	m, err := parseReleaseManifest(raw)
	if err != nil {
		return releaseManifest{}, false, err
	}
	if m.Version != wantVersion {
		return releaseManifest{}, false, fmt.Errorf("manifest version %q != advertised %q", m.Version, wantVersion)
	}
	return m, true, nil
}

// selfUpdate pulls a newer agent binary when the relay advertises one. It
// downloads the binary, VERIFIES the release signature (offline key — see
// updatePubKeyHex) and that the binary runs and reports the advertised version
// (so a corrupt/wrong binary can never replace a working one), atomically
// swaps it in, and exits — systemd (Restart=always) relaunches the new binary.
func selfUpdate() {
	defer func() { _ = recover() }() // never let an update attempt crash the agent
	// Local kill switch — the owner's box, the owner's call. The account-level
	// toggle (Account tab) is enforced relay-side via the ?printer param below;
	// this env var works even if you trust nothing the relay says.
	switch strings.ToLower(env("HOTEND_AGENT_AUTO_UPDATE", "on")) {
	case "off", "0", "false", "no":
		return
	}
	if relayURL == "" {
		return // nothing to ask
	}
	httpBase := relayHTTPBase()
	cl := &http.Client{Timeout: 60 * time.Second}
	// `have` lets the relay record what this printer is actually running, so fleet
	// adoption is visible server-side instead of being assumed.
	resp, err := cl.Get(httpBase + "/agent-bin/version?printer=" + url.QueryEscape(printerID()) + "&have=" + url.QueryEscape(agentVersion))
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	vb, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	resp.Body.Close()
	latest := strings.TrimSpace(string(vb))
	if latest == "" || !versionNewer(latest, agentVersion) {
		return // up to date, opted out (empty), or a downgrade — never go backwards
	}
	// A version this printer already tried and rolled back from is not installed
	// again, however loudly the relay advertises it.
	for _, bad := range readState().BadVersions {
		if bad == latest {
			return
		}
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	// Signed manifest binds version ↔ sha256 per arch. Required when present;
	// 404 is tolerated only so THIS release can itself be installed by older
	// agents that still only check per-binary .sig files. From the next publish
	// onward the manifest is always there and a bad/missing one fails closed.
	manifest, haveManifest, err := fetchReleaseManifest(cl, httpBase, latest)
	if err != nil {
		fmt.Printf("[agent] update %s FAILED release manifest check (%v) — refusing to install\n", latest, err)
		return
	}
	var wantSHA string
	var wantSize int64
	if haveManifest {
		file, ok := manifest.Files[runtime.GOARCH]
		if !ok || file.SHA256 == "" {
			fmt.Printf("[agent] update %s manifest has no entry for %s — refusing to install\n", latest, runtime.GOARCH)
			return
		}
		wantSHA, wantSize = file.SHA256, file.Size
	} else {
		fmt.Printf("[agent] update %s has no signed manifest yet — falling back to per-binary signature\n", latest)
	}
	binName := "hotend-agent-linux-" + runtime.GOARCH
	r2, err := cl.Get(httpBase + "/agent-bin/" + binName)
	if err != nil || r2.StatusCode != 200 {
		if r2 != nil {
			r2.Body.Close()
		}
		return
	}
	data, err := io.ReadAll(io.LimitReader(r2.Body, 64<<20))
	r2.Body.Close()
	if err != nil || len(data) == 0 {
		return
	}
	if wantSHA != "" {
		if got := sha256Hex(data); got != wantSHA {
			fmt.Printf("[agent] update %s FAILED manifest hash check (got %s, want %s) — refusing to install\n", latest, got[:12], wantSHA[:12])
			return
		}
		if wantSize > 0 && int64(len(data)) != wantSize {
			fmt.Printf("[agent] update %s FAILED manifest size check — refusing to install\n", latest)
			return
		}
	}
	// Per-binary release signature — kept as defense in depth beside the manifest.
	// No valid sig, no install. Fail closed and say why: this is the line that
	// makes a compromised relay unable to push code.
	r3, err := cl.Get(httpBase + "/agent-bin/" + binName + ".sig")
	if err != nil || r3.StatusCode != 200 {
		if r3 != nil {
			r3.Body.Close()
		}
		fmt.Printf("[agent] update %s available but unsigned (no .sig) — skipping\n", latest)
		return
	}
	sb, _ := io.ReadAll(io.LimitReader(r3.Body, 1024))
	r3.Body.Close()
	sig, err := hex.DecodeString(strings.TrimSpace(string(sb)))
	if err != nil || len(sig) != ed25519.SignatureSize || !signedByTrustedKey(data, sig) {
		fmt.Printf("[agent] update %s FAILED signature verification — refusing to install\n", latest)
		return
	}
	tmp := self + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return
	}
	// Verify: the new binary must run and report exactly the advertised version.
	out, err := exec.Command(tmp, "-version").Output()
	if err != nil || strings.TrimSpace(string(out)) != latest {
		os.Remove(tmp)
		return
	}
	// Keep the binary we're about to replace. The droplet only ever serves the
	// current release and the downgrade guard refuses older versions, so this copy
	// is the ONLY thing a rollback can use (see revertUpdate).
	if cur, err := os.ReadFile(self); err == nil {
		if err := os.WriteFile(self+prevSuffix+".tmp", cur, 0o755); err == nil {
			os.Rename(self+prevSuffix+".tmp", self+prevSuffix)
		}
	}
	// Mark probation BEFORE the swap: if the rename fails we're still the old
	// binary, and checkPendingUpdate discards a marker that names someone else.
	mutateState(func(s *agentState) {
		s.PendingVersion, s.PendingSince, s.PendingBoots = latest, time.Now().UTC().Format(time.RFC3339), 0
	})
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		mutateState(clearPending)
		return
	}
	fmt.Printf("[agent] updated %s → %s; restarting\n", agentVersion, latest)
	restartSelf()
}

// restartSelf replaces this process with the binary now on disk, so an update
// applies whether or not anything is supervising us.
//
// Exiting and trusting a service manager to start us again is how a printer goes
// dark on a successful update: install-agent.sh falls back to running the agent in
// the FOREGROUND when the host has no systemd user session (locked-down printer
// boards), and there the exit IS the shutdown — no unit, nothing to restart it,
// invisible until someone logs into the printer. exec keeps the same PID, so a
// systemd unit doesn't even notice, and there's no gap where the printer is offline.
func restartSelf() {
	if self, err := os.Executable(); err == nil {
		err = syscall.Exec(self, os.Args, os.Environ())
		fmt.Fprintf(os.Stderr, "[agent] could not exec %s (%v) — exiting for the service manager\n", self, err)
	}
	os.Exit(0)
}

// signedByTrustedKey reports whether sig is a valid release signature over data
// under ANY key the fleet trusts (see updatePubKeysHex — a list so keys can be
// rotated).
func signedByTrustedKey(data, sig []byte) bool {
	for _, h := range updatePubKeysHex {
		pub, err := hex.DecodeString(h)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
			return true
		}
	}
	return false
}

// one WS connection's lifecycle
type conn struct {
	ws        *websocket.Conn
	writeM    sync.Mutex
	bodies    sync.Map // id -> *reqBuf
	localW    sync.Map // id -> *localWS
	smPending sync.Map // spoolman request id -> chan *msg
	cancels   sync.Map // id -> context.CancelFunc for in-flight HTTP fetches (esp. streams)
}

type reqBuf struct {
	method, path, target string
	headers              map[string]string
	buf                  bytes.Buffer
}

type localWS struct {
	ws    *websocket.Conn
	write sync.Mutex
}

func connect(id string) error {
	u := relayURL + "/__relay/agent?printer=" + id
	if claim != "" {
		u += "&claim=" + claim
	}
	// Present the device secret in a header (never the URL, which lands in logs).
	// Empty on a brand-new/legacy agent — the relay issues one on first connect.
	// The version goes up too, so the relay can show which agents are behind.
	hdr := http.Header{"X-Hotend-Agent-Version": {agentVersion}}
	agentSecretMu.RLock()
	sec := agentSecret
	agentSecretMu.RUnlock()
	if sec != "" {
		hdr.Set("Authorization", "Bearer "+sec)
	}
	ws, _, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		return err
	}
	defer ws.Close()
	// Reaching the relay is what proves a freshly installed binary works — it ends
	// the probation started by selfUpdate.
	if connectedOnce.CompareAndSwap(false, true) {
		mutateState(func(s *agentState) {
			if s.PendingVersion == agentVersion {
				clearPending(s)
			}
		})
	}
	fmt.Printf("[agent] connected to %s (printer %s…), proxying %s\n", relayURL, id[:8], localURL)
	c := &conn{ws: ws}
	curConn.Store(c)
	defer curConn.CompareAndSwap(c, nil)
	reportDetectedWebcam(c) // if the webcam's already been detected, tell the relay on (re)connect

	// Detect a dead relay fast. Without this, a relay restart / network drop that
	// doesn't send a clean RST leaves ReadMessage blocked for the whole TCP
	// timeout (minutes) — the agent thinks it's still connected and never
	// reconnects. We send a ping every 30s and require any frame (pong or data)
	// within 70s; otherwise ReadMessage errors and we reconnect immediately.
	const pongWait = 70 * time.Second
	const pingPeriod = 30 * time.Second
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error { ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				// WriteControl is safe to call concurrently with WriteMessage.
				if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			c.localW.Range(func(k, v any) bool { v.(*localWS).ws.Close(); return true })
			return err
		}
		ws.SetReadDeadline(time.Now().Add(pongWait)) // any frame keeps us alive
		var m msg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.dispatch(&m)
	}
}

func (c *conn) send(m *msg) error {
	b, _ := json.Marshal(m)
	c.writeM.Lock()
	err := c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeM.Unlock()
	return err
}

func (c *conn) dispatch(m *msg) {
	switch m.T {
	case "pairing":
		showPairing(m.Code, m.ExpiresInMin)
	case "linked":
		fmt.Println("[agent] linked to account", m.Account)
	case "config":
		// Live config from the relay. Currently just the webcam URL override —
		// empty string clears it back to the installed default.
		webcamMu.Lock()
		webcamOverride = m.WebcamURL
		webcamMu.Unlock()
		if m.WebcamURL != "" {
			fmt.Println("[agent] webcam URL set to", m.WebcamURL)
		} else {
			fmt.Println("[agent] webcam URL reset to default")
		}
	case "device-secret":
		// The relay issued (or backfilled) our device secret — persist it and use it
		// on every reconnect from now on.
		if m.Secret != "" {
			agentSecretMu.Lock()
			agentSecret = m.Secret
			agentSecretMu.Unlock()
			mutateState(func(s *agentState) { s.PrinterID = printerID(); s.DeviceSecret = m.Secret })
			fmt.Println("[agent] stored device secret")
		}
	case "smres": // spoolman response → wake the waiting local HTTP request
		if ch, ok := c.smPending.Load(m.ID); ok {
			select {
			case ch.(chan *msg) <- m:
			default:
			}
		}
	case "req":
		c.bodies.Store(m.ID, &reqBuf{method: m.Method, path: m.Path, target: m.Target, headers: m.Headers})
	case "body":
		if v, ok := c.bodies.Load(m.ID); ok { // request body chunk
			dec, _ := base64.StdEncoding.DecodeString(m.B64)
			v.(*reqBuf).buf.Write(dec)
		} else if v, ok := c.localW.Load(m.ID); ok { // (unused inbound)
			_ = v
		}
	case "end":
		if v, ok := c.bodies.LoadAndDelete(m.ID); ok {
			go c.doFetch(m.ID, v.(*reqBuf))
		}
	case "cancel":
		// Relay says the client hung up (or the stream timed out) — stop pumping.
		// Critical for webcam MJPEG: its body never EOFs, so without this the
		// fetch goroutine would read the camera + encode frames forever.
		if v, ok := c.cancels.Load(m.ID); ok {
			v.(context.CancelFunc)()
		}
	case "wsopen":
		go c.doWSOpen(m.ID, m.Path, m.Headers)
	case "wsmsg":
		if v, ok := c.localW.Load(m.ID); ok {
			dec, _ := base64.StdEncoding.DecodeString(m.B64)
			lw := v.(*localWS)
			mt := websocket.TextMessage
			if m.Binary {
				mt = websocket.BinaryMessage
			}
			lw.write.Lock()
			lw.ws.WriteMessage(mt, dec)
			lw.write.Unlock()
		}
	case "wsclose":
		if v, ok := c.localW.LoadAndDelete(m.ID); ok {
			v.(*localWS).ws.Close()
		}
	}
}

func cleanHeaders(in map[string]string) http.Header {
	h := http.Header{}
	for k, v := range in {
		if stripReq[strings.ToLower(k)] {
			continue
		}
		h.Set(k, v)
	}
	return h
}

func (c *conn) doFetch(id string, r *reqBuf) {
	// Option B: the agent IS the Spoolman integration. Intercept Moonraker's
	// /server/spoolman/* endpoints and answer them from the central store, so a
	// printer needs NO [spoolman] config in Moonraker. Filament usage is tracked
	// by trackFilament() below, not by Moonraker.
	if strings.HasPrefix(pathOnly(r.path), "/server/spoolman/") {
		c.handleMoonrakerSpoolman(id, r)
		return
	}
	var target string
	var err error
	switch r.target {
	case "webcam":
		target, err = buildWebcamURL(effectiveWebcam(), r.path)
	case "webcam-hls":
		target, err = buildWebcamHLS(effectiveWebcam(), r.path)
	default:
		target, err = buildLocalURL(localURL, r.path)
	}
	if err != nil {
		c.errResp(id, err)
		return
	}
	// Cancellable context so a relay `cancel` frame (client hung up / stream timed
	// out) tears the fetch down — vital for the never-EOFing webcam MJPEG stream.
	ctx, cancel := context.WithCancel(context.Background())
	c.cancels.Store(id, cancel)
	defer func() { cancel(); c.cancels.Delete(id) }()

	req, err := http.NewRequestWithContext(ctx, r.method, target, bytes.NewReader(r.buf.Bytes()))
	if err != nil {
		c.errResp(id, err)
		return
	}
	req.Header = cleanHeaders(r.headers)
	// default client auto-decompresses (since we stripped Accept-Encoding), so we
	// forward plain bytes and drop the now-wrong encoding/length headers.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.errResp(id, err)
		return
	}
	defer resp.Body.Close()
	out := map[string]string{}
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-encoding" || lk == "content-length" || lk == "transfer-encoding" {
			continue
		}
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if c.send(&msg{T: "res", ID: id, Status: resp.StatusCode, Headers: out}) != nil {
		return // WS dead — don't start pumping into a closed socket
	}
	b := make([]byte, 32*1024)
	for {
		n, er := resp.Body.Read(b)
		if n > 0 {
			// Stop the instant the WS write fails (relay restart / client gone) —
			// the old code ignored this and streamed the camera to a dead socket
			// for the life of the process.
			if c.send(&msg{T: "body", ID: id, B64: base64.StdEncoding.EncodeToString(b[:n])}) != nil {
				return
			}
		}
		if er != nil {
			break
		}
	}
	c.send(&msg{T: "end", ID: id})
}

// serveSpoolman runs a tiny Spoolman-compatible HTTP endpoint on localhost.
// Moonraker's [spoolman] server points here; every /api/v1/* call is forwarded
// over the relay to Hotend's central per-account store. Because MOONRAKER drives
// it, filament usage is recorded for every print from any source.
func serveSpoolman() {
	addr := "127.0.0.1:" + spoolmanPort
	mux := http.NewServeMux()
	mux.HandleFunc("/", spoolmanHandler)
	fmt.Printf("[agent] spoolman endpoint on http://%s (Moonraker [spoolman] server:)\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, "[agent] spoolman endpoint failed:", err)
	}
}

var smUpgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func spoolmanHandler(w http.ResponseWriter, r *http.Request) {
	// Moonraker opens a websocket (ws://…/api/v1/spool) for live spool updates and
	// gates "spoolman_connected" (and usage sync) on it. We don't push events —
	// the central store is the source of truth — so just accept it and hold it
	// open (gorilla answers pings during ReadMessage) so Moonraker stays connected.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		ws, err := smUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}
	c := curConn.Load()
	if c == nil {
		http.Error(w, `{"message":"agent offline"}`, http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	id := "sm-" + strconv.FormatUint(atomic.AddUint64(&smSeq, 1), 36)
	ch := make(chan *msg, 1)
	c.smPending.Store(id, ch)
	defer c.smPending.Delete(id)
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	req := &msg{T: "sm", ID: id, Method: r.Method, Path: path}
	if len(body) > 0 {
		req.Body = json.RawMessage(body)
	}
	c.send(req)
	select {
	case res := <-ch:
		status := res.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		if len(res.Body) > 0 {
			w.Write(res.Body)
		} else {
			w.Write([]byte("{}"))
		}
	case <-time.After(20 * time.Second):
		http.Error(w, `{"message":"spoolman upstream timeout"}`, http.StatusGatewayTimeout)
	}
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// buildLocalURL turns a relay-supplied request path into a concrete local URL,
// refusing anything that isn't a single-slash-rooted path. The honest relay only
// ever sends "/...", but the agent must not trust it: a path like "//evil/x",
// "/\evil", or an absolute "http://…" could otherwise redirect the agent's fetch
// off-box (SSRF) or open a WS to an attacker. We parse the trusted base and set
// only the path/query, never string-concatenating attacker-influenced input into
// the authority.
// buildWebcamURL proxies to the webcam stream. Unlike the Moonraker path (which
// is a fixed API), the webcam lives at whatever HOTEND_WEBCAM_URL points to —
// mjpg-streamer at http://host:8080/ (stream at /?action=stream) OR crowsnest at
// http://host/webcam/ (stream at /webcam/?action=stream). So we PRESERVE the
// configured base path and only carry the request's query (action=stream|snapshot).
func buildWebcamURL(base, p string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	rawQuery := ""
	if i := strings.IndexByte(p, '?'); i >= 0 {
		rawQuery = p[i+1:]
	}
	if u.Path == "" {
		u.Path = "/"
	} else if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/" // crowsnest needs the trailing slash before ?action=
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

// buildWebcamHLS appends an HLS sub-path (the playlist or a segment) onto the
// configured webcam base, so camera-streamer's index.m3u8 + its .ts segments both
// resolve under the same base directory. Same single-slash-rooted guard as the
// local path (no //, /\, or absolute) so a malicious relay frame can't redirect
// the fetch off-box; the base host/scheme comes only from the trusted webcam URL.
func buildWebcamHLS(base, p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "", fmt.Errorf("bad hls path %q", p)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	rawPath, rawQuery := p, ""
	if i := strings.IndexByte(p, '?'); i >= 0 {
		rawPath, rawQuery = p[:i], p[i+1:]
	}
	u.Path = strings.TrimRight(u.Path, "/") + rawPath
	u.RawQuery = rawQuery
	return u.String(), nil
}

// detectWebcam resolves the printer's camera. FIRST it asks Moonraker's webcam
// registry (what Mainsail/Fluidd populate — knows crowsnest/camera-streamer/go2rtc/
// MediaMTX + protocol); if that's empty (camera streaming but not registered), it
// falls back to probing the common streamer layouts directly. Returns (baseURL,
// "mjpeg"|"hls") or ("",""). Best-effort — failure keeps the install-time default.
func detectWebcam() (string, string) {
	if u, t := detectWebcamFromMoonraker(); u != "" {
		return u, t
	}
	return probeWebcam()
}

// probeWebcam is the registry fallback: try the layouts crowsnest / camera-streamer
// / mjpg-streamer serve — an MJPEG snapshot (native, preferred) first, then an HLS
// playlist — on the Moonraker host (and :8080). Returns the working base + type.
func probeWebcam() (string, string) {
	cl := &http.Client{Timeout: 4 * time.Second}
	host := webcamHost()
	if host == "" {
		return "", ""
	}
	bases := []string{host + "/webcam", host + "/webcam2", host + "/usbcam", host + "/usbcam2", host + ":8080", host + "/cam", host}
	for _, b := range bases { // MJPEG first — Hotend renders it natively (no hls.js)
		if okJPEG(cl, strings.TrimRight(b, "/")+"/?action=snapshot") {
			return strings.TrimRight(b, "/"), "mjpeg"
		}
	}
	for _, b := range bases { // then HLS (camera-streamer / go2rtc-style playlists)
		if okM3U8(cl, strings.TrimRight(b, "/")+"/index.m3u8") {
			return strings.TrimRight(b, "/"), "hls"
		}
	}
	return "", ""
}

func okJPEG(cl *http.Client, u string) bool {
	resp, err := cl.Get(u)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "image") {
		return false
	}
	b := make([]byte, 2)
	n, _ := io.ReadFull(resp.Body, b)
	return n == 2 && b[0] == 0xFF && b[1] == 0xD8 // JPEG magic bytes
}

func okM3U8(cl *http.Client, u string) bool {
	resp, err := cl.Get(u)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()
	b := make([]byte, 7)
	n, _ := io.ReadFull(resp.Body, b)
	return n == 7 && string(b) == "#EXTM3U"
}

// detectWebcamFromMoonraker asks Moonraker's webcam registry — the list Mainsail/
// Fluidd populate — for the configured stream + protocol.
func detectWebcamFromMoonraker() (string, string) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(localURL + "/server/webcams/list")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return "", ""
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Webcams []struct {
				Service     string `json:"service"`
				StreamURL   string `json:"stream_url"`
				SnapshotURL string `json:"snapshot_url"`
			} `json:"webcams"`
		} `json:"result"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil || len(out.Result.Webcams) == 0 {
		return "", ""
	}
	w := out.Result.Webcams[0]
	host := webcamHost() // scheme://host of Moonraker, minus its API port
	svc := strings.ToLower(w.Service)
	// HLS services: 'hlsstream', 'hls', camera-streamer's HLS variants.
	if strings.Contains(svc, "hls") {
		return absWebcam(host, hlsDir(w.StreamURL)), "hls"
	}
	// Everything else is MJPEG-family (mjpegstreamer[-adaptive], camera-streamer
	// mjpeg, uv4l, …) — the base is the snapshot/stream path minus its query.
	raw := w.SnapshotURL
	if raw == "" {
		raw = w.StreamURL
	}
	base := absWebcam(host, stripQuery(raw))
	if base == "" {
		return "", ""
	}
	return base, "mjpeg"
}

func stripQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return strings.TrimRight(u, "/")
}

// hlsDir turns a playlist URL (…/index.m3u8) into its containing directory.
func hlsDir(u string) string {
	u = stripQuery(u)
	if i := strings.LastIndexByte(u, '/'); i >= 0 && strings.Contains(u[i:], ".m3u8") {
		u = u[:i]
	}
	return u
}

// webcamHost is scheme://host (no port) of Moonraker. Webcams are served by the
// Mainsail/Fluidd web server (usually :80), not Moonraker's :7125.
func webcamHost() string {
	u, err := url.Parse(localURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Hostname()
}

// absWebcam joins a (possibly Moonraker-relative) webcam path with the host; an
// already-absolute URL is used as-is.
func absWebcam(host, p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return strings.TrimRight(p, "/")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return host + p
}

// reportDetectedWebcam tells the relay what we auto-detected, so it can configure
// the printer's camera (URL + protocol) if the owner hasn't set one manually.
func reportDetectedWebcam(c *conn) {
	detectedMu.RLock()
	u, t := detectedWebcamURL, detectedWebcamType
	detectedMu.RUnlock()
	if u != "" {
		c.send(&msg{T: "detected-webcam", WebcamURL: u, WebcamType: t})
	}
}

func buildLocalURL(base, p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "", fmt.Errorf("bad request path %q", p)
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	rawPath, rawQuery := p, ""
	if i := strings.IndexByte(p, '?'); i >= 0 {
		rawPath, rawQuery = p[:i], p[i+1:]
	}
	u.Path = rawPath
	u.RawQuery = rawQuery
	return u.String(), nil
}

// smRequest sends a Spoolman request over the relay to the central store and
// waits for the response (same framed channel Moonraker's own calls use).
func (c *conn) smRequest(method, path string, body json.RawMessage) (*msg, error) {
	id := "sm-" + strconv.FormatUint(atomic.AddUint64(&smSeq, 1), 36)
	ch := make(chan *msg, 1)
	c.smPending.Store(id, ch)
	defer c.smPending.Delete(id)
	req := &msg{T: "sm", ID: id, Method: method, Path: path}
	if len(body) > 0 {
		req.Body = body
	}
	c.send(req)
	select {
	case res := <-ch:
		return res, nil
	case <-time.After(20 * time.Second):
		return nil, fmt.Errorf("spoolman upstream timeout")
	}
}

// activeSpoolID reads the account's active spool id from the central store.
func (c *conn) activeSpoolID() *int {
	res, err := c.smRequest("GET", "/api/v1/_hotend/active", nil)
	if err != nil || res == nil || len(res.Body) == 0 {
		return nil
	}
	var r struct {
		SpoolID *int `json:"spool_id"`
	}
	json.Unmarshal(res.Body, &r)
	return r.SpoolID
}

// handleMoonrakerSpoolman emulates Moonraker's /server/spoolman/* endpoints from
// the central store, so the app + Moonraker need no [spoolman] config at all.
func (c *conn) handleMoonrakerSpoolman(id string, r *reqBuf) {
	path := pathOnly(r.path)
	respond := func(status int, obj any) {
		b, _ := json.Marshal(obj)
		c.send(&msg{T: "res", ID: id, Status: status, Headers: map[string]string{"content-type": "application/json"}})
		c.send(&msg{T: "body", ID: id, B64: base64.StdEncoding.EncodeToString(b)})
		c.send(&msg{T: "end", ID: id})
	}
	switch {
	case strings.HasSuffix(path, "/server/spoolman/status"):
		respond(200, map[string]any{"result": map[string]any{
			"spoolman_connected": true, "spool_id": c.activeSpoolID(), "pending_reports": []any{},
		}})
	case strings.HasSuffix(path, "/server/spoolman/spool_id"):
		if r.method == "POST" || r.method == "PUT" {
			c.smRequest("PUT", "/api/v1/_hotend/active", json.RawMessage(r.buf.Bytes()))
			var sp struct {
				SpoolID *int `json:"spool_id"`
			}
			json.Unmarshal(r.buf.Bytes(), &sp)
			respond(200, map[string]any{"result": map[string]any{"spool_id": sp.SpoolID}})
		} else {
			respond(200, map[string]any{"result": map[string]any{"spool_id": c.activeSpoolID()}})
		}
	case strings.HasSuffix(path, "/server/spoolman/proxy"):
		var p struct {
			RequestMethod string          `json:"request_method"`
			Path          string          `json:"path"`
			Body          json.RawMessage `json:"body"`
		}
		if json.Unmarshal(r.buf.Bytes(), &p) != nil || p.Path == "" {
			respond(400, map[string]any{"error": "bad proxy request"})
			return
		}
		// The app sends "/v1/spool"; the central store expects "/api/v1/spool".
		res, err := c.smRequest(p.RequestMethod, "/api"+p.Path, p.Body)
		if err != nil {
			respond(502, map[string]any{"error": err.Error()})
			return
		}
		status := res.Status
		if status == 0 {
			status = 200
		}
		var inner any
		if len(res.Body) > 0 {
			json.Unmarshal(res.Body, &inner)
		}
		respond(status, map[string]any{"result": inner})
	default:
		respond(404, map[string]any{"error": "not found"})
	}
}

// startFilamentTracking picks exactly ONE usage-counting path. When Moonraker
// has a [spoolman] section (the installer wires it to our local endpoint),
// Moonraker itself reports usage through serveSpoolman() — counting here too
// would deduct every print TWICE. Only a printer WITHOUT that config (bridge
// mode, no moonraker.conf edit) gets the agent's own print_stats tracking.
func startFilamentTracking() {
	if moonrakerHasSpoolman() {
		fmt.Println("[agent] Moonraker [spoolman] configured — filament usage comes from Moonraker; agent-side tracking off")
		return
	}
	fmt.Println("[agent] no Moonraker [spoolman] — agent tracks filament usage itself")
	trackFilament()
}

// moonrakerHasSpoolman asks Moonraker for its config and reports whether a
// [spoolman] section exists. Connection errors retry forever (Moonraker may
// still be booting — same pattern as the webcam probe); any HTTP answer decides:
// non-200 or unparseable is treated as "not configured" so bridge-mode printers
// still get tracking.
func moonrakerHasSpoolman() bool {
	cl := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := cl.Get(localURL + "/server/config")
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var r struct {
			Result struct {
				Config map[string]json.RawMessage `json:"config"`
			} `json:"result"`
		}
		if json.Unmarshal(body, &r) != nil {
			return false
		}
		_, ok := r.Result.Config["spoolman"]
		return ok
	}
}

// trackFilament subscribes to Moonraker's print_stats and reports filament used
// to the printer's active spool — replacing Moonraker's own [spoolman] usage sync,
// so filament is tracked for every print with just the agent (no printer config).
func trackFilament() {
	wsURL := strings.Replace(localURL, "http", "ws", 1) + "/websocket"
	for {
		ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		ws.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","method":"printer.objects.subscribe","params":{"objects":{"print_stats":["filament_used","state"]}},"id":9901}`))
		var curFU, lastSent float64
		var curState string
		// Report the ABSOLUTE filament_used; the store computes the delta against
		// its own watermark, so a reconnect/restart re-reporting the same value
		// can't double-count. lastSent is only a send throttle, not correctness.
		report := func(absFU float64) {
			if cc := curConn.Load(); cc != nil {
				body, _ := json.Marshal(map[string]float64{"filament_used": absFU})
				cc.smRequest("PUT", "/api/v1/_hotend/active/use", body)
			}
		}
		for {
			_, data, er := ws.ReadMessage()
			if er != nil {
				break
			}
			fu, st, hasFU, hasState := parsePrintStats(data)
			prevState := curState
			if hasFU {
				curFU = fu
			}
			if hasState {
				curState = st
			}
			if curState == "printing" || curState == "paused" {
				if curFU-lastSent >= 50 || curFU < lastSent { // send every ~50mm, or on a new-print reset
					report(curFU)
					lastSent = curFU
				}
			}
			if (prevState == "printing" || prevState == "paused") &&
				(curState == "complete" || curState == "cancelled" || curState == "standby" || curState == "error") {
				report(curFU) // flush the final total at print end
				lastSent = curFU
			}
		}
		ws.Close()
		time.Sleep(3 * time.Second)
	}
}

// parsePrintStats pulls filament_used/state from a subscribe response or a
// notify_status_update (which carries only the changed fields).
func parsePrintStats(data []byte) (fu float64, state string, hasFU, hasState bool) {
	type ps struct {
		FilamentUsed *float64 `json:"filament_used"`
		State        *string  `json:"state"`
	}
	var env struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
		Result struct {
			Status struct {
				PS *ps `json:"print_stats"`
			} `json:"status"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &env) != nil {
		return
	}
	apply := func(p *ps) {
		if p == nil {
			return
		}
		if p.FilamentUsed != nil {
			fu, hasFU = *p.FilamentUsed, true
		}
		if p.State != nil {
			state, hasState = *p.State, true
		}
	}
	if env.Result.Status.PS != nil {
		apply(env.Result.Status.PS)
		return
	}
	if env.Method == "notify_status_update" && len(env.Params) > 0 {
		var p struct {
			PS *ps `json:"print_stats"`
		}
		if json.Unmarshal(env.Params[0], &p) == nil {
			apply(p.PS)
		}
	}
	return
}

func (c *conn) errResp(id string, err error) {
	c.send(&msg{T: "res", ID: id, Status: 502, Headers: map[string]string{"content-type": "text/plain"}})
	c.send(&msg{T: "body", ID: id, B64: base64.StdEncoding.EncodeToString([]byte("relay agent: " + err.Error()))})
	c.send(&msg{T: "end", ID: id})
}

func (c *conn) doWSOpen(id, path string, headers map[string]string) {
	target, err := buildLocalURL(strings.Replace(localURL, "http", "ws", 1), path)
	if err != nil {
		c.send(&msg{T: "wsclose", ID: id})
		return
	}
	lw, _, err := websocket.DefaultDialer.Dial(target, cleanHeaders(headers))
	if err != nil {
		c.send(&msg{T: "wsclose", ID: id})
		return
	}
	l := &localWS{ws: lw}
	c.localW.Store(id, l)
	for {
		mt, data, er := lw.ReadMessage()
		if er != nil {
			break
		}
		c.send(&msg{T: "body", ID: id, B64: base64.StdEncoding.EncodeToString(data), Binary: mt == websocket.BinaryMessage})
	}
	c.localW.Delete(id)
	c.send(&msg{T: "wsclose", ID: id})
}

func showPairing(code string, mins int) {
	fmt.Println("\n  ┌─────────────────────────────────────────────┐")
	fmt.Println("  │  Link this printer to your Hotend account:    │")
	fmt.Printf("  │     pairing code:  %-25s│\n", code)
	fmt.Printf("  │  Open the Hotend app → Add printer → enter it │\n")
	fmt.Printf("  │  (code expires in %d min)%*s│\n", mins, 20-len(fmt.Sprint(mins)), "")
	fmt.Println("  └─────────────────────────────────────────────┘")
}
