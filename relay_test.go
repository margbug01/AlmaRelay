package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	m.Run()
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

var (
	secret     = hex.EncodeToString(random(32))
	routingKey = base64.RawURLEncoding.EncodeToString(random(16))
	hash       = func() string { s := sha256.Sum256([]byte(routingKey)); return hex.EncodeToString(s[:]) }()
)

func setup(t *testing.T, mutate ...func(*Config)) (*Relay, string) {
	t.Helper()
	cfg := Config{RegistrationKey: secret}
	for _, m := range mutate {
		m(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r)
	t.Cleanup(func() { r.Stop(); srv.Close() })
	return r, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialWith(t *testing.T, d *websocket.Dialer, url string, h http.Header) *websocket.Conn {
	t.Helper()
	ws, _, err := d.Dial(url, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func dial(t *testing.T, url string) *websocket.Conn {
	return dialWith(t, websocket.DefaultDialer, url, nil)
}

func phoneURL(base string) string { return base + "/c/" + routingKey + "/__alma_e2e__" }

func nextRaw(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("desktop got message type %d", kind)
	}
	return data
}

func next(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(nextRaw(t, ws), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func send(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func registerWith(t *testing.T, d *websocket.Dialer, base, key string) (*websocket.Conn, map[string]any) {
	t.Helper()
	ws := dialWith(t, d, base+"/agent?th="+hash, nil)
	send(t, ws, map[string]any{"t": "register", "tokenHash": hash, "key": key, "name": "Test desktop"})
	return ws, next(t, ws)
}

func register(t *testing.T, base string) *websocket.Conn {
	t.Helper()
	ws, reply := registerWith(t, websocket.DefaultDialer, base, secret)
	if reply["t"] != "registered" {
		t.Fatalf("registration reply %v", reply)
	}
	return ws
}

// openPhone connects a phone and acknowledges its tunnel from the desktop.
func openPhone(t *testing.T, base string, desktop *websocket.Conn) (*websocket.Conn, string) {
	t.Helper()
	phone := dial(t, phoneURL(base))
	id := next(t, desktop)["id"].(string)
	send(t, desktop, map[string]any{"t": "wopened", "id": id})
	return phone, id
}

func rejected(t *testing.T, url string, status int, h http.Header) {
	t.Helper()
	ws, res, err := websocket.DefaultDialer.Dial(url, h)
	if err == nil {
		ws.Close()
		t.Fatalf("%s: upgrade succeeded, want %d", url, status)
	}
	if res == nil || res.StatusCode != status {
		t.Fatalf("%s: %v, want %d", url, err, status)
	}
}

// closeCode reads until the connection ends and returns the close code.
func closeCode(t *testing.T, ws *websocket.Conn) int {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		var ce *websocket.CloseError
		var ne net.Error
		switch {
		case err == nil:
		case errors.As(err, &ce):
			return ce.Code
		case errors.As(err, &ne) && ne.Timeout():
			t.Fatal("connection did not close")
		default:
			return websocket.CloseAbnormalClosure
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

func TestRequiresStrongKey(t *testing.T) {
	if _, err := New(Config{RegistrationKey: "short"}); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestHealthCheckDoesNotLeakPairings(t *testing.T) {
	_, base := setup(t)
	res, err := http.Get("http" + strings.TrimPrefix(base, "ws") + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if len(body) != 1 || body["ok"] != true {
		t.Fatalf("healthz %v", body)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	r, base := setup(t)
	ws, reply := registerWith(t, websocket.DefaultDialer, base, "wrong")
	if reply["t"] != "error" {
		t.Fatalf("reply %v", reply)
	}
	if code := closeCode(t, ws); code != 1008 {
		t.Fatalf("close %d", code)
	}
	if r.Stats().Agents != 0 {
		t.Fatal("agent left behind")
	}
}

func TestRegistrationQueryCannotDisagreeWithFrame(t *testing.T) {
	_, base := setup(t)
	ws := dial(t, base+"/agent?th="+hash)
	send(t, ws, map[string]any{"t": "register", "tokenHash": strings.Repeat("a", 64), "key": secret})
	if code := closeCode(t, ws); code != 1008 {
		t.Fatalf("close %d", code)
	}
}

func TestRejectsUnknownRoutesAndForeignOrigins(t *testing.T) {
	_, base := setup(t, func(c *Config) { c.PublicOrigin = "https://relay.example.com" })
	rejected(t, phoneURL(base), 503, nil)
	rejected(t, base+"/c/"+routingKey+"/api/settings", 404, nil)
	rejected(t, base+"/agent?th="+hash, 403, http.Header{"Origin": {"https://other.example.com"}})
	rejected(t, base+"/agent?th=bad", 400, nil)
	ws := dialWith(t, websocket.DefaultDialer, base+"/agent?th="+hash, http.Header{"Origin": {"https://relay.example.com"}})
	ws.Close()
}

func TestRegistrationTimeoutAndMalformedFrames(t *testing.T) {
	r, base := setup(t, func(c *Config) { c.RegistrationTimeout = 30 * time.Millisecond })
	if code := closeCode(t, dial(t, base+"/agent?th="+hash)); code != 1008 {
		t.Fatalf("idle close %d", code)
	}
	invalid := dial(t, base+"/agent?th="+hash)
	invalid.WriteMessage(websocket.TextMessage, []byte("null"))
	if code := closeCode(t, invalid); code != 1008 {
		t.Fatalf("invalid close %d", code)
	}
	if r.Stats().Agents != 0 {
		t.Fatal("agent left behind")
	}
}

func TestForwardsQueuesAndIsolatesPhones(t *testing.T) {
	r, base := setup(t)
	desktop := register(t, base)
	phone := dial(t, phoneURL(base))
	open := next(t, desktop)
	if open["t"] != "wopen" || open["path"] != "/__alma_e2e__" {
		t.Fatalf("open %v", open)
	}
	phone.WriteMessage(websocket.TextMessage, []byte("early handshake"))
	send(t, desktop, map[string]any{"t": "wopened", "id": open["id"]})
	if m := next(t, desktop); m["s"] != "early handshake" || m["id"] != open["id"] {
		t.Fatalf("queued %v", m)
	}
	send(t, desktop, map[string]any{"t": "wmsg", "id": open["id"], "s": "handshake response"})
	if kind, data, _ := phone.ReadMessage(); kind != websocket.TextMessage || string(data) != "handshake response" {
		t.Fatalf("phone got %d %q", kind, data)
	}

	second, id := openPhone(t, base, desktop)
	if id == open["id"] {
		t.Fatal("tunnel ids collide")
	}
	payload := random(4096)
	second.WriteMessage(websocket.BinaryMessage, payload)
	up := next(t, desktop)
	if b, _ := base64.StdEncoding.DecodeString(up["b"].(string)); up["id"] != id || !bytes.Equal(b, payload) {
		t.Fatal("binary upstream mismatch")
	}
	send(t, desktop, map[string]any{"t": "wmsg", "id": id, "b": up["b"]})
	if kind, data, _ := second.ReadMessage(); kind != websocket.BinaryMessage || !bytes.Equal(data, payload) {
		t.Fatal("binary downstream mismatch")
	}
	second.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""))
	if m := next(t, desktop); m["t"] != "wclose" || m["id"] != id {
		t.Fatalf("close %v", m)
	}
	if n := r.Stats().Tunnels; n != 1 {
		t.Fatalf("%d tunnels", n)
	}
}

func TestReplacementClosesOldDesktopOnly(t *testing.T) {
	r, base := setup(t)
	first := register(t, base)
	phone, _ := openPhone(t, base, first)
	register(t, base)
	if code := closeCode(t, first); code != 4000 {
		t.Fatalf("old desktop close %d", code)
	}
	closeCode(t, phone)
	if s := r.Stats(); s.Agents != 1 || s.Tunnels != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestDesktopLossClosesPhones(t *testing.T) {
	r, base := setup(t)
	desktop := register(t, base)
	phone := dial(t, phoneURL(base))
	next(t, desktop)
	desktop.Close()
	closeCode(t, phone)
	waitFor(t, "cleanup", func() bool { s := r.Stats(); return s.Agents == 0 && s.Tunnels == 0 })
}

func TestLimitsPhonesAndCancelsUnacknowledgedOpens(t *testing.T) {
	r, base := setup(t, func(c *Config) { c.MaxTunnels = 1; c.OpenTimeout = 100 * time.Millisecond })
	desktop := register(t, base)
	phone := dial(t, phoneURL(base))
	next(t, desktop)
	rejected(t, phoneURL(base), 429, nil)
	if code := closeCode(t, phone); code != 1012 {
		t.Fatalf("close %d", code)
	}
	if n := r.Stats().Tunnels; n != 0 {
		t.Fatalf("%d tunnels", n)
	}
}

func TestHeartbeatToleratesDelayedPongsButRemovesSilentConnections(t *testing.T) {
	r, base := setup(t, func(c *Config) {
		c.HeartbeatInterval = 20 * time.Millisecond
		c.HeartbeatTimeout = 200 * time.Millisecond
	})
	ws := dial(t, base+"/agent?th="+hash)
	var respond atomic.Bool
	respond.Store(true)
	ws.SetPingHandler(func(data string) error {
		if respond.Load() {
			time.AfterFunc(60*time.Millisecond, func() {
				ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
			})
		}
		return nil
	})
	send(t, ws, map[string]any{"t": "register", "tokenHash": hash, "key": secret})
	next(t, ws)
	closed := make(chan struct{})
	go func() {
		ws.SetReadDeadline(time.Time{})
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}()
	time.Sleep(350 * time.Millisecond)
	select {
	case <-closed:
		t.Fatal("delayed pongs were treated as dead")
	default:
	}
	respond.Store(false)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("silent connection kept")
	}
	waitFor(t, "agent removal", func() bool { return r.Stats().Agents == 0 })
}

func TestAPNsUnsupported(t *testing.T) {
	_, base := setup(t)
	desktop := register(t, base)
	send(t, desktop, map[string]any{"t": "push", "id": "test-push", "notifications": []any{}})
	got, _ := json.Marshal(next(t, desktop))
	if string(got) != `{"id":"test-push","results":[],"t":"pushed","unsupported":true}` {
		t.Fatalf("push reply %s", got)
	}
}

func TestOversizedDesktopMessagesRejected(t *testing.T) {
	// Compressed, 2 KiB of "x" is a few bytes on the wire: the decompressed size must be bounded too.
	for _, compress := range []bool{false, true} {
		_, base := setup(t, func(c *Config) { c.MaxPayload = 1024 })
		ws, _ := registerWith(t, &websocket.Dialer{EnableCompression: compress}, base, secret)
		ws.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte("x"), 2048))
		if code := closeCode(t, ws); code != 1009 {
			t.Fatalf("compress=%v: close %d", compress, code)
		}
	}
}

func TestOversizedPhoneMessageOnlyClosesItsTunnel(t *testing.T) {
	r, base := setup(t, func(c *Config) { c.MaxPayload = 1024 })
	desktop := register(t, base)
	phone, id := openPhone(t, base, desktop)

	// The largest binary message still fits after base64 and the JSON wrapper.
	fits := random(r.phoneLimit)
	phone.WriteMessage(websocket.BinaryMessage, fits)
	raw := nextRaw(t, desktop)
	var m map[string]string
	json.Unmarshal(raw, &m)
	if b, _ := base64.StdEncoding.DecodeString(m["b"]); len(raw) > 1024 || !bytes.Equal(b, fits) {
		t.Fatalf("largest message: %d byte frame", len(raw))
	}

	phone.WriteMessage(websocket.BinaryMessage, random(r.phoneLimit+1))
	if code := closeCode(t, phone); code != 1009 {
		t.Fatalf("binary: phone close %d", code)
	}
	if m := next(t, desktop); m["t"] != "wclose" || m["id"] != id {
		t.Fatalf("binary: desktop got %v", m)
	}

	// Text fits the phone limit but doubles when JSON-escaped.
	escaped, id2 := openPhone(t, base, desktop)
	escaped.WriteMessage(websocket.TextMessage, bytes.Repeat([]byte(`"`), 600))
	if code := closeCode(t, escaped); code != 1009 {
		t.Fatalf("text: phone close %d", code)
	}
	if m := next(t, desktop); m["t"] != "wclose" || m["id"] != id2 {
		t.Fatalf("text: desktop got %v", m)
	}

	openPhone(t, base, desktop) // the desktop is still there
	if s := r.Stats(); s.Agents != 1 || s.Tunnels != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestSlowDesktopGetsBackpressureNotDisconnected(t *testing.T) {
	r, base := setup(t, func(c *Config) { c.MaxBuffered = 64 << 10 })
	desktop := register(t, base)
	phone, id := openPhone(t, base, desktop)
	const count, size = 128, 128 << 10 // 16 MiB, more than socket buffers hold
	go func() {
		for i := range count {
			if phone.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{byte(i)}, size)) != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond) // the desktop is not reading

	r.mu.Lock()
	sl := r.agents[hash]
	r.mu.Unlock()
	sl.p.mu.Lock()
	queued := sl.p.bytes
	sl.p.mu.Unlock()
	if one := len(wrap(id, websocket.BinaryMessage, make([]byte, size))); queued > one {
		t.Fatalf("desktop queue grew to %d bytes, limit is one %d byte message", queued, one)
	}

	for i := range count {
		var m map[string]string
		json.Unmarshal(nextRaw(t, desktop), &m)
		b, _ := base64.StdEncoding.DecodeString(m["b"])
		if len(b) != size || b[0] != byte(i) || b[size-1] != byte(i) {
			t.Fatalf("message %d out of order or damaged", i)
		}
	}
	if s := r.Stats(); s.Agents != 1 || s.Tunnels != 1 {
		t.Fatalf("stats %+v", s)
	}
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

func TestDesktopLegCompressesBase64(t *testing.T) {
	payload := random(1 << 20)
	for _, compress := range []bool{false, true} {
		_, base := setup(t)
		var wire atomic.Int64
		d := &websocket.Dialer{EnableCompression: compress, NetDial: func(network, addr string) (net.Conn, error) {
			c, err := net.Dial(network, addr)
			return countingConn{c, &wire}, err
		}}
		desktop, _ := registerWith(t, d, base, secret)
		phone, _, err := (&websocket.Dialer{EnableCompression: true}).Dial(phoneURL(base), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer phone.Close()
		id := next(t, desktop)["id"]
		send(t, desktop, map[string]any{"t": "wopened", "id": id})

		before := wire.Load()
		phone.WriteMessage(websocket.BinaryMessage, payload)
		up := next(t, desktop)
		ratio := float64(wire.Load()-before) / float64(len(payload))
		if b, _ := base64.StdEncoding.DecodeString(up["b"].(string)); !bytes.Equal(b, payload) {
			t.Fatal("upstream mismatch")
		}
		if (compress && ratio > 1.02) || (!compress && ratio < 1.33) {
			t.Fatalf("compress=%v: desktop read %.3f bytes per payload byte", compress, ratio)
		}
		t.Logf("compress=%v: desktop leg %.3f bytes per payload byte", compress, ratio)

		send(t, desktop, map[string]any{"t": "wmsg", "id": id, "b": up["b"]})
		phone.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, data, err := phone.ReadMessage(); err != nil || !bytes.Equal(data, payload) {
			t.Fatalf("downstream mismatch: %v", err)
		}
	}
}

func TestPhoneLegIsNotCompressed(t *testing.T) {
	_, base := setup(t)
	register(t, base)
	ws, res, err := (&websocket.Dialer{EnableCompression: true}).Dial(phoneURL(base), nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.Close()
	if ext := res.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		t.Fatalf("phone negotiated %q; its traffic is ciphertext", ext)
	}
}
