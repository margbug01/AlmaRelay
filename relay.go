package main

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

type Config struct {
	RegistrationKey     string
	PublicOrigin        string
	MaxPayload          int // largest WebSocket message in either direction
	MaxBuffered         int // per-connection send queue
	MaxConnections      int
	MaxTunnels          int // phones per desktop
	RegistrationTimeout time.Duration
	OpenTimeout         time.Duration
	HeartbeatInterval   time.Duration
	HeartbeatTimeout    time.Duration
}

type Stats struct{ Agents, Connections, Tunnels int }

type Relay struct {
	cfg        Config
	key        [32]byte
	origin     string
	phoneLimit int
	agentUp    websocket.Upgrader
	phoneUp    websocket.Upgrader
	done       chan struct{}

	mu       sync.Mutex // guards everything below and all slot/tunnel fields
	agents   map[string]*slot
	peers    map[*peer]struct{}
	attempts map[string]*rate
	stopping bool
}

type rate struct {
	start time.Time
	count int
}

// slot is a registered desktop.
type slot struct {
	p       *peer
	hash    string
	tunnels map[string]*tunnel
}

// tunnel is one phone connection bridged through a desktop.
type tunnel struct {
	id          string
	p           *peer
	slot        *slot
	opened      bool
	queue       [][]byte
	queuedBytes int
	timer       *time.Timer
	closed      atomic.Bool // read without Relay.mu while waiting for room on the desktop
}

var (
	phoneRoute = regexp.MustCompile(`^/c/([A-Za-z0-9_-]{22})/__alma_e2e__$`)
	tokenHash  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	errTooBig  = errors.New("message too big")
)

// Phone messages reach the desktop wrapped as {"t":"wmsg","id":"<uuid>","b":"<base64>"}.
const (
	wmsgHead     = `{"t":"wmsg","id":"`
	wrapOverhead = len(wmsgHead) + 36 + len(`","b":"`) + len(`"}`)
)

func New(cfg Config) (*Relay, error) {
	if len(cfg.RegistrationKey) < 24 {
		return nil, errors.New("RELAY_REGISTRATION_KEY must contain at least 24 characters")
	}
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	defDur := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	def(&cfg.MaxPayload, 16<<20)
	def(&cfg.MaxBuffered, 32<<20)
	def(&cfg.MaxConnections, 256)
	def(&cfg.MaxTunnels, 16)
	defDur(&cfg.RegistrationTimeout, 5*time.Second)
	defDur(&cfg.OpenTimeout, 10*time.Second)
	defDur(&cfg.HeartbeatInterval, 15*time.Second)
	defDur(&cfg.HeartbeatTimeout, 60*time.Second)
	if cfg.MaxPayload <= wrapOverhead+4 {
		return nil, errors.New("MaxPayload is too small")
	}
	r := &Relay{
		cfg:  cfg,
		key:  sha256.Sum256([]byte(cfg.RegistrationKey)),
		done: make(chan struct{}),
		// Largest phone binary message whose base64 wrapper still fits in MaxPayload.
		phoneLimit: (cfg.MaxPayload - wrapOverhead) / 4 * 3,
		agents:     map[string]*slot{},
		peers:      map[*peer]struct{}{},
		attempts:   map[string]*rate{},
	}
	if cfg.PublicOrigin != "" {
		u, err := url.Parse(cfg.PublicOrigin)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, errors.New("invalid PUBLIC_ORIGIN")
		}
		r.origin = u.Scheme + "://" + u.Host
	}
	allow := func(*http.Request) bool { return true } // Origin is checked before upgrading
	// Only the desktop leg is worth compressing: it carries base64, phones carry ciphertext.
	r.agentUp = websocket.Upgrader{CheckOrigin: allow, EnableCompression: true, HandshakeTimeout: 10 * time.Second}
	r.phoneUp = websocket.Upgrader{CheckOrigin: allow, HandshakeTimeout: 10 * time.Second}
	go r.heartbeat()
	return r, nil
}

func (r *Relay) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Stats{Agents: len(r.agents), Connections: len(r.peers)}
	for _, sl := range r.agents {
		s.Tunnels += len(sl.tunnels)
	}
	return s
}

func (r *Relay) Stop() {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		return
	}
	r.stopping = true
	close(r.done)
	peers := make([]*peer, 0, len(r.peers))
	for p := range r.peers {
		peers = append(peers, p)
	}
	r.mu.Unlock()
	for _, p := range peers {
		p.kill()
	}
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if websocket.IsWebSocketUpgrade(req) {
		r.upgrade(w, req)
		return
	}
	if req.Method == http.MethodGet && req.RequestURI == "/healthz" {
		r.mu.Lock()
		stopping := r.stopping
		r.mu.Unlock()
		h.Set("Content-Type", "application/json")
		if stopping {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		fmt.Fprintf(w, `{"ok":%t}`, !stopping)
		return
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	if req.RequestURI == "/" {
		io.WriteString(w, "Private Alma relay. Use the Alma mobile app with end-to-end encryption enabled.\n")
		return
	}
	w.WriteHeader(http.StatusNotFound)
	io.WriteString(w, "Not found. This relay only supports encrypted WebSocket tunnels.\n")
}

func reject(w http.ResponseWriter, status int) {
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
}

func (r *Relay) upgrade(w http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()
	isAgent := path == "/agent"
	route := phoneRoute.FindStringSubmatch(path)
	var hash string
	if isAgent {
		hash = req.URL.Query().Get("th")
	} else if route != nil {
		sum := sha256.Sum256([]byte(route[1]))
		hash = hex.EncodeToString(sum[:])
	}

	r.mu.Lock()
	if r.stopping || len(r.peers) >= r.cfg.MaxConnections {
		r.mu.Unlock()
		reject(w, http.StatusServiceUnavailable)
		return
	}
	address, _, _ := net.SplitHostPort(req.RemoteAddr)
	now := time.Now()
	rt := r.attempts[address]
	if rt == nil || now.Sub(rt.start) > time.Minute {
		rt = &rate{start: now}
		r.attempts[address] = rt
	}
	rt.count++
	status := 0
	var sl *slot
	switch {
	case rt.count > 120:
		status = http.StatusTooManyRequests
	case req.Header.Get("Origin") != "" && req.Header.Get("Origin") != r.origin:
		status = http.StatusForbidden // r.origin is "" when unset, so any Origin is refused
	case !isAgent && route == nil:
		status = http.StatusNotFound
	case !tokenHash.MatchString(hash):
		status = http.StatusBadRequest
	case !isAgent:
		sl = r.agents[hash]
		if sl == nil || sl.p.isClosing() {
			status = http.StatusServiceUnavailable
		} else if len(sl.tunnels) >= r.cfg.MaxTunnels {
			status = http.StatusTooManyRequests
		}
	}
	r.mu.Unlock()
	if status != 0 {
		reject(w, status)
		return
	}

	up := &r.phoneUp
	if isAgent {
		up = &r.agentUp
	}
	ws, err := up.Upgrade(w, req, nil) // writes its own error response
	if err != nil {
		return
	}
	p := newPeer(ws)
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		p.kill()
		return
	}
	r.peers[p] = struct{}{}
	r.mu.Unlock()
	ws.SetReadDeadline(time.Now().Add(r.cfg.HeartbeatTimeout))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(r.cfg.HeartbeatTimeout)) })
	ws.SetPingHandler(func(data string) error { p.push(websocket.PongMessage, []byte(data), 0); return nil })
	go p.writeLoop()

	role := "phone"
	var code int
	if isAgent {
		role = "desktop"
		deflate := strings.Contains(strings.Join(req.Header.Values("Sec-WebSocket-Extensions"), ","), "permessage-deflate")
		ws.SetReadLimit(int64(r.cfg.MaxPayload))
		// Payloads are base64 of ciphertext: no repeats for LZ77 to find, but 6 bits
		// per character for Huffman. Levels 1-6 store such blocks uncompressed.
		ws.SetCompressionLevel(flate.HuffmanOnly)
		code = r.runAgent(p, hash, deflate)
	} else {
		ws.SetReadLimit(int64(r.phoneLimit))
		code = r.runPhone(p, sl)
	}
	p.kill()
	r.mu.Lock()
	delete(r.peers, p)
	r.mu.Unlock()
	log.Printf("Relay connection closed %s %d", role, code)
}

// read returns the next message, bounding its decompressed size by limit.
func read(p *peer, limit int) (int, []byte, error) {
	kind, rd, err := p.ws.NextReader()
	if err != nil {
		return 0, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(rd, int64(limit)+1))
	if err == nil && len(data) > limit {
		err = errTooBig
	}
	if err == nil && kind == websocket.TextMessage && !utf8.Valid(data) {
		err = errors.New("invalid UTF-8")
	}
	return kind, data, err
}

// readError logs the failure and returns the close code to report.
func readError(p *peer, err error) int {
	var ce *websocket.CloseError
	var ne net.Error
	switch {
	case errors.As(err, &ce):
		return ce.Code
	case errors.As(err, &ne) && ne.Timeout():
		log.Print("Relay heartbeat timed out")
	case errors.Is(err, errTooBig), errors.Is(err, websocket.ErrReadLimit):
		if errors.Is(err, errTooBig) { // gorilla already sent this for ErrReadLimit
			p.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, ""), time.Now().Add(time.Second))
		}
		log.Print("Relay socket error message-too-big")
		return websocket.CloseMessageTooBig
	case p.isClosing():
		// We closed or killed it ourselves.
	default:
		log.Print("Relay socket error")
	}
	return websocket.CloseAbnormalClosure
}

func (r *Relay) runAgent(p *peer, hash string, deflate bool) int {
	var sl *slot
	timer := time.AfterFunc(r.cfg.RegistrationTimeout, func() {
		r.mu.Lock()
		if sl == nil {
			p.close(websocket.ClosePolicyViolation, "Registration timeout")
		}
		r.mu.Unlock()
	})
	defer func() {
		timer.Stop()
		r.mu.Lock()
		if sl != nil {
			r.removeAgent(sl)
		}
		r.mu.Unlock()
	}()
	for {
		kind, data, err := read(p, r.cfg.MaxPayload)
		if err != nil {
			return readError(p, err)
		}
		if p.isClosing() {
			continue // we already closed it; don't let a late frame register
		}
		var m map[string]json.RawMessage
		if kind != websocket.TextMessage || json.Unmarshal(data, &m) != nil || m == nil {
			p.close(websocket.ClosePolicyViolation, "Invalid protocol frame")
			continue
		}
		t, _ := str(m, "t")
		var payload []byte
		payloadKind, valid := 0, false
		if t == "wmsg" { // decode before taking the lock; this can be 16 MiB
			payloadKind, payload, valid = wmsgPayload(m)
		}

		r.mu.Lock()
		switch {
		case sl == nil:
			th, _ := str(m, "tokenHash")
			key, ok := str(m, "key")
			sum := sha256.Sum256([]byte(key))
			if t != "register" || th != hash || !ok || len(key) > 4096 || subtle.ConstantTimeCompare(sum[:], r.key[:]) != 1 {
				p.pushJSON(map[string]any{"t": "error", "message": "Registration denied"})
				p.close(websocket.ClosePolicyViolation, "Registration denied")
				break
			}
			timer.Stop()
			if prev := r.agents[hash]; prev != nil {
				r.removeAgent(prev)
				prev.p.close(4000, "Another desktop took over this pairing")
			}
			sl = &slot{p: p, hash: hash, tunnels: map[string]*tunnel{}}
			r.agents[hash] = sl
			p.pushJSON(map[string]any{"t": "registered"})
			log.Printf("Relay desktop registered, compression %t", deflate)
		case r.agents[hash] != sl:
			// Replaced by another desktop; ignore what is still in flight.
		case t == "push":
			id, _ := str(m, "id")
			if rs := []rune(id); len(rs) > 128 {
				id = string(rs[:128])
			}
			p.pushJSON(map[string]any{"t": "pushed", "id": id, "unsupported": true, "results": []any{}})
		default:
			id, _ := str(m, "id")
			if tn := sl.tunnels[id]; tn != nil {
				r.tunnelFrame(sl, tn, t, payloadKind, payload, valid)
			}
		}
		r.mu.Unlock()
	}
}

// tunnelFrame handles a desktop frame for one tunnel. Caller holds r.mu.
func (r *Relay) tunnelFrame(sl *slot, tn *tunnel, t string, kind int, payload []byte, valid bool) {
	switch t {
	case "wopened":
		if tn.opened {
			return
		}
		tn.opened = true
		tn.timer.Stop()
		for _, q := range tn.queue {
			if !sl.p.push(websocket.TextMessage, q, 0) {
				break
			}
		}
		tn.queue, tn.queuedBytes = nil, 0
	case "wmsg":
		if !tn.opened || !valid {
			r.removeTunnel(sl, tn, true, websocket.CloseServiceRestart)
			return
		}
		if len(payload) > r.cfg.MaxPayload || !tn.p.push(kind, payload, r.cfg.MaxBuffered) {
			tn.p.kill() // phone is not keeping up; its close cleans the tunnel up
		}
	case "wclose":
		r.removeTunnel(sl, tn, false, websocket.CloseNormalClosure)
	case "werr":
		r.removeTunnel(sl, tn, false, websocket.CloseInternalServerErr)
	}
}

// wmsgPayload extracts exactly one of "s" (text) or "b" (base64 binary).
func wmsgPayload(m map[string]json.RawMessage) (int, []byte, bool) {
	s, sOK := str(m, "s")
	b, bOK := str(m, "b")
	_, hasS := m["s"]
	_, hasB := m["b"]
	switch {
	case sOK && !hasB:
		return websocket.TextMessage, []byte(s), true
	case bOK && !hasS && len(b)%4 == 0 && !strings.ContainsAny(b, "\r\n"):
		data, err := base64.StdEncoding.DecodeString(b)
		return websocket.BinaryMessage, data, err == nil
	}
	return 0, nil, false
}

func str(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	return s, json.Unmarshal(raw, &s) == nil
}

func (r *Relay) runPhone(p *peer, sl *slot) int {
	tn := &tunnel{id: uuid(), p: p, slot: sl}
	r.mu.Lock()
	if r.agents[sl.hash] != sl || !sl.p.pushJSON(map[string]any{"t": "wopen", "id": tn.id, "path": "/__alma_e2e__"}) {
		r.mu.Unlock()
		p.close(websocket.CloseServiceRestart, "Tunnel closed")
		return readUntilClosed(p)
	}
	sl.tunnels[tn.id] = tn
	tn.timer = time.AfterFunc(r.cfg.OpenTimeout, func() {
		r.mu.Lock()
		if !tn.opened {
			r.removeTunnel(sl, tn, true, websocket.CloseServiceRestart)
		}
		r.mu.Unlock()
	})
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.removeTunnel(sl, tn, true, websocket.CloseServiceRestart)
		r.mu.Unlock()
	}()
	for {
		kind, data, err := read(p, r.phoneLimit)
		if err != nil {
			return readError(p, err)
		}
		msg := wrap(tn.id, kind, data)
		r.mu.Lock()
		switch {
		case sl.tunnels[tn.id] != tn:
		case len(msg) > r.cfg.MaxPayload:
			// Only text can get here (JSON escaping); binary is bounded by phoneLimit.
			r.removeTunnel(sl, tn, true, websocket.CloseMessageTooBig)
		case !tn.opened:
			tn.queuedBytes += len(data)
			if tn.queuedBytes > 65536 || len(tn.queue) >= 8 {
				r.removeTunnel(sl, tn, true, websocket.CloseMessageTooBig)
			} else {
				tn.queue = append(tn.queue, msg)
			}
		default:
			r.mu.Unlock()
			// Wait for the desktop to drain instead of dropping it with every other phone.
			ok, waited := sl.p.pushWait(msg, r.cfg.MaxBuffered, &tn.closed)
			if waited { // we were not reading; don't count that against the heartbeat
				p.ws.SetReadDeadline(time.Now().Add(r.cfg.HeartbeatTimeout))
			}
			if ok {
				continue
			}
			r.mu.Lock()
			r.removeTunnel(sl, tn, true, websocket.CloseServiceRestart)
		}
		r.mu.Unlock()
	}
}

// readUntilClosed drains a peer we already decided to close.
func readUntilClosed(p *peer) int {
	for {
		if _, _, err := p.ws.NextReader(); err != nil {
			return readError(p, err)
		}
	}
}

// wrap builds the desktop frame for a phone message.
func wrap(id string, kind int, data []byte) []byte {
	if kind == websocket.BinaryMessage {
		out := make([]byte, 0, wrapOverhead+base64.StdEncoding.EncodedLen(len(data)))
		out = append(append(append(out, wmsgHead...), id...), `","b":"`...)
		out = base64.StdEncoding.AppendEncode(out, data)
		return append(out, `"}`...)
	}
	var b bytes.Buffer
	b.WriteString(wmsgHead + id + `","s":`)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(string(data))
	b.Truncate(b.Len() - 1) // Encode appends a newline
	b.WriteByte('}')
	return b.Bytes()
}

// removeTunnel is idempotent. Caller holds r.mu.
func (r *Relay) removeTunnel(sl *slot, tn *tunnel, notify bool, code int) {
	if sl.tunnels[tn.id] != tn {
		return
	}
	delete(sl.tunnels, tn.id)
	tn.closed.Store(true)
	if tn.timer != nil {
		tn.timer.Stop()
	}
	tn.queue = nil
	if notify {
		sl.p.pushJSON(map[string]any{"t": "wclose", "id": tn.id})
	} else {
		sl.p.wake() // a phone may be waiting for room on this desktop
	}
	tn.p.close(code, "Tunnel closed")
}

// removeAgent closes every tunnel of a desktop. Caller holds r.mu.
func (r *Relay) removeAgent(sl *slot) {
	if r.agents[sl.hash] == sl {
		delete(r.agents, sl.hash)
	}
	for _, tn := range sl.tunnels {
		r.removeTunnel(sl, tn, false, websocket.CloseServiceRestart)
	}
}

func (r *Relay) heartbeat() {
	tick := time.NewTicker(r.cfg.HeartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-tick.C:
		}
		r.mu.Lock()
		for p := range r.peers {
			p.push(websocket.PingMessage, nil, 0)
		}
		for addr, rt := range r.attempts {
			if time.Since(rt.start) > time.Minute {
				delete(r.attempts, addr)
			}
		}
		r.mu.Unlock()
	}
}

func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// peer owns one WebSocket. Everything it sends goes through one FIFO queue and
// one writer goroutine, like the socket buffer of the Node version.
type peer struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	cond    sync.Cond // queue changed, room freed, or peer closed
	queue   []outFrame
	bytes   int
	closing bool // close frame queued or connection gone: accept nothing more
	dead    bool
}

type outFrame struct {
	kind int
	data []byte
}

func newPeer(ws *websocket.Conn) *peer {
	p := &peer{ws: ws}
	p.cond.L = &p.mu
	return p
}

// push queues a frame. limit > 0 refuses to grow the queue past limit bytes.
func (p *peer) push(kind int, data []byte, limit int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || (limit > 0 && p.bytes+len(data) > limit) {
		return false
	}
	p.queue = append(p.queue, outFrame{kind, data})
	p.bytes += len(data)
	p.cond.Broadcast()
	return true
}

func (p *peer) pushJSON(v any) bool {
	data, _ := json.Marshal(v)
	return p.push(websocket.TextMessage, data, 0)
}

// pushWait queues a text frame, waiting while the queue is over limit.
// It gives up when the peer closes or stop becomes true.
func (p *peer) pushWait(data []byte, limit int, stop *atomic.Bool) (ok, waited bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.closing && !stop.Load() && p.bytes > 0 && p.bytes+len(data) > limit {
		waited = true
		p.cond.Wait()
	}
	if p.closing || stop.Load() {
		return false, waited
	}
	p.queue = append(p.queue, outFrame{websocket.TextMessage, data})
	p.bytes += len(data)
	p.cond.Broadcast()
	return true, waited
}

func (p *peer) wake() {
	p.mu.Lock()
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *peer) isClosing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closing
}

// close sends a close frame after what is already queued and drops the
// connection one second later if the other side has not closed it by then.
func (p *peer) close(code int, reason string) {
	p.mu.Lock()
	if !p.closing {
		p.closing = true
		p.queue = append(p.queue, outFrame{websocket.CloseMessage, websocket.FormatCloseMessage(code, reason)})
		p.cond.Broadcast()
	}
	p.mu.Unlock()
	time.AfterFunc(time.Second, p.kill)
}

func (p *peer) kill() {
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return
	}
	p.dead, p.closing = true, true
	p.queue, p.bytes = nil, 0
	p.cond.Broadcast()
	p.mu.Unlock()
	p.ws.Close()
}

func (p *peer) writeLoop() {
	for {
		p.mu.Lock()
		for len(p.queue) == 0 && !p.dead {
			p.cond.Wait()
		}
		if p.dead {
			p.mu.Unlock()
			return
		}
		f := p.queue[0]
		p.queue[0] = outFrame{}
		p.queue = p.queue[1:]
		p.mu.Unlock()

		var err error
		if f.kind == websocket.TextMessage || f.kind == websocket.BinaryMessage {
			p.ws.EnableWriteCompression(len(f.data) >= 1024) // no-op unless negotiated
			err = p.ws.WriteMessage(f.kind, f.data)
		} else {
			err = p.ws.WriteControl(f.kind, f.data, time.Now().Add(10*time.Second))
		}
		p.mu.Lock()
		if !p.dead {
			p.bytes -= len(f.data)
		}
		p.cond.Broadcast()
		p.mu.Unlock()
		if err != nil {
			p.kill()
			return
		}
	}
}
