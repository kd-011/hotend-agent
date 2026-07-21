// Hotend printer agent — a single static binary with no runtime dependencies.
// It dials OUT to the Hotend relay over one outbound WebSocket and bridges
// whatever the relay sends to the local Moonraker API (and the webcam). Nothing
// listens for inbound connections from the internet, so there is no port to
// forward and nothing on your LAN is exposed.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
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
	"time"

	"github.com/gorilla/websocket"
)

// Bump on every agent release. Agents auto-update to whatever /agent-bin/version
// advertises (see selfUpdate). Date-based so "newer" is obvious.
const agentVersion = "2026.07.20"

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
	statePath = env("HOTEND_AGENT_STATE", filepath.Join(home(), ".hotend-agent.json"))
)

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
	Secret       string            `json:"secret,omitempty"` // device secret issued by the relay
	Body         json.RawMessage   `json:"body,omitempty"`   // spoolman request/response body
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
	stateMu       sync.Mutex
	agentPrinter  string
	agentSecretMu sync.RWMutex
	agentSecret   string
)

type agentState struct {
	PrinterID    string `json:"printerId"`
	DeviceSecret string `json:"deviceSecret,omitempty"`
}

func loadState() agentState {
	var s agentState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &s)
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
	b, _ := json.Marshal(s)
	os.WriteFile(statePath, b, 0o600)
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(agentVersion)
		return
	}
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
	agentPrinter = id
	agentSecret = st.DeviceSecret
	go serveSpoolman() // local Spoolman endpoint (Option A: Moonraker [spoolman] → store)
	go trackFilament() // Option B: agent tracks filament + serves spoolman itself
	go func() {        // self-update: check on boot, then every 6h
		for {
			selfUpdate()
			time.Sleep(6 * time.Hour)
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

// selfUpdate pulls a newer agent binary when the relay advertises one. It
// downloads to a temp file, VERIFIES it runs and reports the advertised version
// (so a corrupt/wrong binary can never replace a working one), atomically
// swaps it in, and exits — systemd (Restart=always) relaunches the new binary.
func selfUpdate() {
	defer func() { _ = recover() }() // never let an update attempt crash the agent
	httpBase := strings.Replace(strings.Replace(relayURL, "wss://", "https://", 1), "ws://", "http://", 1)
	cl := &http.Client{Timeout: 60 * time.Second}
	resp, err := cl.Get(httpBase + "/agent-bin/version")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	vb, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	resp.Body.Close()
	latest := strings.TrimSpace(string(vb))
	if latest == "" || latest == agentVersion {
		return // up to date (or version endpoint not published)
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	r2, err := cl.Get(httpBase + "/agent-bin/hotend-agent-linux-" + runtime.GOARCH)
	if err != nil || r2.StatusCode != 200 {
		if r2 != nil {
			r2.Body.Close()
		}
		return
	}
	defer r2.Body.Close()
	tmp := self + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return
	}
	if _, err := io.Copy(f, io.LimitReader(r2.Body, 64<<20)); err != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	f.Close()
	// Verify: the new binary must run and report exactly the advertised version.
	out, err := exec.Command(tmp, "-version").Output()
	if err != nil || strings.TrimSpace(string(out)) != latest {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		return
	}
	fmt.Printf("[agent] updated %s → %s; restarting\n", agentVersion, latest)
	os.Exit(0)
}

// one WS connection's lifecycle
type conn struct {
	ws        *websocket.Conn
	writeM    sync.Mutex
	bodies    sync.Map // id -> *reqBuf
	localW    sync.Map // id -> *localWS
	smPending sync.Map // spoolman request id -> chan *msg
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

func connect(printerID string) error {
	u := relayURL + "/__relay/agent?printer=" + printerID
	if claim != "" {
		u += "&claim=" + claim
	}
	// Present the device secret in a header (never the URL, which lands in logs).
	// Empty on a brand-new/legacy agent — the relay issues one on first connect.
	var hdr http.Header
	agentSecretMu.RLock()
	sec := agentSecret
	agentSecretMu.RUnlock()
	if sec != "" {
		hdr = http.Header{"Authorization": {"Bearer " + sec}}
	}
	ws, _, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		return err
	}
	defer ws.Close()
	fmt.Printf("[agent] connected to %s (printer %s…), proxying %s\n", relayURL, printerID[:8], localURL)
	c := &conn{ws: ws}
	curConn.Store(c)
	defer curConn.CompareAndSwap(c, nil)

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

func (c *conn) send(m *msg) {
	b, _ := json.Marshal(m)
	c.writeM.Lock()
	c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeM.Unlock()
}

func (c *conn) dispatch(m *msg) {
	switch m.T {
	case "pairing":
		showPairing(m.Code, m.ExpiresInMin)
	case "linked":
		fmt.Println("[agent] linked to account", m.Account)
	case "device-secret":
		// The relay issued (or backfilled) our device secret — persist it and use it
		// on every reconnect from now on.
		if m.Secret != "" {
			agentSecretMu.Lock()
			agentSecret = m.Secret
			agentSecretMu.Unlock()
			saveState(agentState{PrinterID: agentPrinter, DeviceSecret: m.Secret})
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
	if r.target == "webcam" {
		target, err = buildWebcamURL(webcamURL, r.path)
	} else {
		target, err = buildLocalURL(localURL, r.path)
	}
	if err != nil {
		c.errResp(id, err)
		return
	}
	req, err := http.NewRequest(r.method, target, bytes.NewReader(r.buf.Bytes()))
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
	c.send(&msg{T: "res", ID: id, Status: resp.StatusCode, Headers: out})
	b := make([]byte, 32*1024)
	for {
		n, er := resp.Body.Read(b)
		if n > 0 {
			c.send(&msg{T: "body", ID: id, B64: base64.StdEncoding.EncodeToString(b[:n])})
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

// trackFilament subscribes to Moonraker's print_stats and reports filament used
// to the account's active spool — replacing Moonraker's own [spoolman] usage sync,
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
