// Package wshub is a WebSocket server written from scratch against RFC 6455,
// with nothing but the Go standard library.
//
// SSE pushes one way over plain HTTP. A WebSocket is a REAL two-way socket
// that starts life as an ordinary HTTP GET and then "upgrades": the client
// sends a random nonce in Sec-WebSocket-Key, the server answers with
//
//	Sec-WebSocket-Accept: base64(SHA1(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
//
// and from that moment the TCP connection stops speaking HTTP and starts
// speaking framed messages. That GUID is fixed by the RFC and is not a
// secret; hashing with it only proves the server UNDERSTOOD the protocol
// instead of blindly echoing a header, which is enough to stop a cache or a
// naive proxy from being talked into acting like a WebSocket endpoint.
//
// In Go the upgrade needs http.Hijacker. net/http wants to own the
// request/response lifecycle, and Hijack is how it hands the raw net.Conn
// back, so this package can write the 101 by hand and then own the socket for
// the rest of its life. After a hijack, nothing may touch the ResponseWriter
// again — every rejection below therefore happens BEFORE the hijack.
//
// # The frame format
//
// Every frame is a two-byte header plus optional extensions:
//
//	 0                   1                   2                   3
//	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
//	+-+-+-+-+-------+-+-------------+-------------------------------+
//	|F|R|R|R|opcode |M| payload len |   extended payload length     |
//	|I|S|S|S| (4b)  |A|   (7 bits)  |  (16 or 64 bits, if 126/127)  |
//	|N|1|2|3|       |S|             |                               |
//	+-+-+-+-+-------+-+-------------+-------------------------------+
//	| masking key (4 bytes, client -> server only) | payload data.. |
//	+---------------------------------------------------------------+
//
// The length is the fiddly part: 7 bits when the payload fits in 125 bytes,
// otherwise the escape value 126 (real length in the next 2 bytes) or 127
// (the next 8). Opcodes: 0x0 continuation, 0x1 text, 0x2 binary, 0x8 close,
// 0x9 ping, 0xA pong. The three RSV bits are reserved for negotiated
// extensions; with none negotiated they must be zero.
//
// Direction matters. Client -> server frames MUST be masked: four random
// bytes XORed over the payload, which exists to stop an attacker scripting a
// browser into emitting bytes that a transparent proxy could mistake for a
// real HTTP request. Server -> client frames MUST NOT be masked. So this
// package always writes plain frames and always unmasks what it reads — and
// the masking key is also why you cannot find the next frame boundary without
// parsing the length properly.
//
// # What the hub is for, and what Go forces it to get right
//
// Dashboards listen; they do not chat. The hub is a fan-out pusher: one
// Broadcast turns a typed event into one text frame delivered to every
// connected socket. Inbound frames only ever need to be understood well
// enough to answer a ping, honour a close, and step cleanly over anything
// else to reach the next frame.
//
// The Node original got away with four things that a single-threaded event
// loop hides and Go does not:
//
//  1. CONCURRENT WRITES. Broadcast is called from several goroutines at once
//     (a 2s stats ticker, request handlers, the AI review loop). Two
//     goroutines writing frames to the same TCP connection interleave bytes
//     and corrupt the stream for good — the client cannot resynchronise
//     because frame boundaries are implied by the lengths it just lost. So
//     every connection has exactly ONE writer goroutine, and every other
//     goroutine only ever hands it a fully-formed frame down a channel.
//
//  2. SLOW CLIENTS. A dashboard that stops reading fills its socket buffer,
//     and then a write blocks forever. If the broadcaster wrote directly,
//     one wedged laptop would stall the whole gateway. The writer goroutine
//     instead drains a BUFFERED channel; when that buffer is full the client
//     is further behind than we are willing to carry, so it is DROPPED
//     (closed with 1013 "try again later") rather than allowed to block
//     anyone. Broadcast therefore never blocks on the network.
//
//  3. HALF-OPEN CONNECTIONS. Every write carries a deadline, so a peer that
//     vanished without a FIN (the laptop that went into a tunnel) cannot
//     wedge its writer goroutine indefinitely.
//
//  4. HOSTILE INPUT. Frame parsing is bounded and total: reserved bits set,
//     an unmasked client frame, an oversized or fragmented control frame, an
//     absurd declared length or a truncated header all close that ONE
//     connection and nothing else. Nothing in this package panics on network
//     input, and no declared length is ever allocated before it is checked.
package wshub

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// magicGUID is the constant every RFC 6455 server appends to the client's
// Sec-WebSocket-Key before hashing. Fixed by the spec, not a secret.
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Header bits and opcodes, straight out of RFC 6455 section 5.2.
const (
	finBit  byte = 0x80 // last frame of a message
	rsvBits byte = 0x70 // RSV1..RSV3; must be zero with no extension negotiated
	maskBit byte = 0x80 // set in byte 1 when a masking key follows

	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// Close codes (RFC 6455 section 7.4.1) this package emits.
const (
	closeNormal        uint16 = 1000 // graceful, nothing wrong
	closeGoingAway     uint16 = 1001 // the server is shutting down
	closeProtocolError uint16 = 1002 // the peer sent something illegal
	closeMessageTooBig uint16 = 1009 // the peer sent more than we will buffer
	closeTryAgainLater uint16 = 1013 // the peer fell too far behind; reconnect
)

const (
	// defaultSendBuffer is how many frames may be queued for one client
	// before it is considered hopeless. Frames here are small JSON events, so
	// this is a couple of seconds of the 2s stats ticker plus a burst of
	// decisions — enough to ride out a GC pause, far short of a memory leak.
	defaultSendBuffer = 64

	// defaultWriteTimeout bounds a single frame write. A peer that cannot
	// absorb one small frame in this long is gone, whatever TCP still thinks.
	defaultWriteTimeout = 5 * time.Second

	// controlBuffer holds pongs and close frames. Control traffic is tiny and
	// rare; if even this is full the peer is already being torn down.
	controlBuffer = 4

	// closeGrace is how long the writer goroutine gets to flush a Close frame
	// before the socket is torn down under it.
	closeGrace = 250 * time.Millisecond

	// maxInboundPayload caps a single client frame. Dashboards send pings and
	// closes; anything approaching a megabyte is a mistake or an attack, and
	// an uncapped declared length is a 16-exabyte allocation waiting to
	// happen.
	maxInboundPayload = 1 << 20
)

// Errors returned by inbound frame parsing. Each one closes exactly one
// connection; none of them is fatal to the gateway.
var (
	// ErrUnmaskedClientFrame reports a client frame without the mask bit set.
	// RFC 6455 requires clients to mask, so an unmasked frame is either a
	// broken client or something that is not a WebSocket client at all.
	ErrUnmaskedClientFrame = errors.New("wshub: client frame is not masked")

	// ErrReservedBits reports a frame with RSV1..RSV3 set when no extension
	// was negotiated.
	ErrReservedBits = errors.New("wshub: reserved frame bits set")

	// ErrControlFrameTooLong reports a control frame (close/ping/pong) with a
	// payload over the 125-byte limit the RFC puts on control frames.
	ErrControlFrameTooLong = errors.New("wshub: control frame payload exceeds 125 bytes")

	// ErrFragmentedControlFrame reports a control frame without FIN set.
	// Control frames may never be fragmented.
	ErrFragmentedControlFrame = errors.New("wshub: control frame is fragmented")

	// ErrPayloadTooLarge reports a declared payload length beyond what this
	// hub is willing to buffer for one inbound frame.
	ErrPayloadTooLarge = errors.New("wshub: frame payload too large")

	// ErrUnknownOpcode reports an opcode the RFC reserves for future use.
	ErrUnknownOpcode = errors.New("wshub: unknown frame opcode")
)

// ComputeAccept returns the Sec-WebSocket-Accept value for a client's
// Sec-WebSocket-Key: base64(SHA1(key + magic GUID)). This is the whole of the
// handshake proof — the client compares it against the same computation and
// aborts the connection if it does not match.
func ComputeAccept(secWebSocketKey string) string {
	sum := sha1.Sum([]byte(secWebSocketKey + magicGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// EncodeTextFrame wraps s in a single unmasked server text frame (FIN set,
// opcode 0x1), choosing the 7-bit, 16-bit or 64-bit length form as needed.
// Server frames are never masked, so the payload is copied through untouched.
func EncodeTextFrame(s string) []byte {
	return encodeTextFrame([]byte(s))
}

// encodeTextFrame is EncodeTextFrame over bytes, so Broadcast can frame the
// output of json.Marshal without a string round-trip.
func encodeTextFrame(payload []byte) []byte {
	n := len(payload)
	var out []byte
	switch {
	case n < 126:
		// The whole length rides in the low 7 bits of byte 1.
		out = make([]byte, 2+n)
		out[0] = finBit | opText
		out[1] = byte(n)
		copy(out[2:], payload)
	case n < 1<<16:
		// 126 is an escape: the real length is the next 2 bytes, big-endian.
		out = make([]byte, 4+n)
		out[0] = finBit | opText
		out[1] = 126
		binary.BigEndian.PutUint16(out[2:4], uint16(n))
		copy(out[4:], payload)
	default:
		// 127 is the other escape: the real length is the next 8 bytes.
		out = make([]byte, 10+n)
		out[0] = finBit | opText
		out[1] = 127
		binary.BigEndian.PutUint64(out[2:10], uint64(n))
		copy(out[10:], payload)
	}
	return out
}

// encodeControlFrame builds an unmasked control frame. Control payloads are
// capped at 125 bytes by the RFC, so an over-long reason is truncated rather
// than turned into an illegal frame.
func encodeControlFrame(opcode byte, payload []byte) []byte {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	out := make([]byte, 2+len(payload))
	out[0] = finBit | opcode
	out[1] = byte(len(payload))
	copy(out[2:], payload)
	return out
}

// encodeCloseFrame builds a Close frame carrying a status code and an
// optional human reason, per RFC 6455 section 5.5.1.
func encodeCloseFrame(code uint16, reason string) []byte {
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	payload = append(payload, reason...)
	return encodeControlFrame(opClose, payload)
}

// isControl reports whether an opcode is a control opcode (0x8..0xF), which
// the RFC restricts to unfragmented frames of at most 125 bytes.
func isControl(opcode byte) bool { return opcode&0x8 != 0 }

// knownOpcode reports whether an opcode is one RFC 6455 defines. 0x3..0x7 and
// 0xB..0xF are reserved for future use, and receiving one is a protocol error
// rather than something to skip over.
func knownOpcode(opcode byte) bool {
	switch opcode {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
		return true
	default:
		return false
	}
}

// envelope is the wire shape of every broadcast: {"type":..., "data":...}.
// Field order matches the JSON the Node gateway emitted, so existing
// dashboard clients need no change.
type envelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub is the set of connected dashboards plus the fan-out that feeds them.
// The zero value is not usable; call New.
type Hub struct {
	// authorize decides whether an upgrade request may have a socket. It runs
	// before the hijack, so a rejection is still a normal HTTP response.
	authorize func(r *http.Request) bool

	// sendBuffer and writeTimeout are fields rather than constants purely so
	// tests can shrink them; New installs the production defaults.
	sendBuffer   int
	writeTimeout time.Duration

	mu     sync.RWMutex
	conns  map[*conn]struct{}
	closed bool
}

// New builds an empty Hub. authorize is the predicate that decides who gets a
// socket; it may be nil to accept every upgrade. The token rides the query
// string rather than a header because the browser WebSocket API cannot set
// headers on an upgrade request — so authorize typically reads
// r.URL.Query().Get("token") and validates it as a bearer token would be.
func New(authorize func(r *http.Request) bool) *Hub {
	return &Hub{
		authorize:    authorize,
		sendBuffer:   defaultSendBuffer,
		writeTimeout: defaultWriteTimeout,
		conns:        make(map[*conn]struct{}),
	}
}

// HandleUpgrade performs the RFC 6455 handshake and registers the resulting
// socket with the hub. Wire it to the upgrade route (for example /ws) of an
// ordinary net/http mux.
//
// It rejects, without hijacking anything, in four cases: authorize said no
// (401), Sec-WebSocket-Key is missing or blank (400), Sec-WebSocket-Version
// is not 13 (400), or the hub is shutting down (503). Only once every check
// has passed does it take the connection away from net/http via
// http.Hijacker, write the 101 by hand, and hand the socket to a reader and a
// writer goroutine.
//
// HandleUpgrade returns as soon as the socket is registered; the connection
// outlives the handler, which is exactly why the ResponseWriter must not be
// touched afterwards.
func (h *Hub) HandleUpgrade(w http.ResponseWriter, r *http.Request) {
	if h.authorize != nil && !h.authorize(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return
	}
	if version := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); version != "13" {
		// The RFC asks a server that refuses a version to advertise the ones
		// it does speak, so a client can retry instead of guessing.
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported Sec-WebSocket-Version", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	closed := h.closed
	h.mu.RUnlock()
	if closed {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection hijacking unsupported", http.StatusInternalServerError)
		return
	}
	nc, brw, err := hj.Hijack()
	if err != nil {
		// The hijack failed, so net/http still owns the response and a normal
		// error is still legal to write.
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}

	if err := writeHandshake(nc, key, h.writeTimeout); err != nil {
		_ = nc.Close()
		return
	}

	// brw.Reader may already hold bytes the client pipelined behind the
	// handshake, so inbound framing must read through it and never from the
	// raw conn. Writes go straight to the conn from the single writer
	// goroutine, so the buffered writer is deliberately left unused.
	h.register(nc, brw.Reader)
}

// writeHandshake sends the 101 Switching Protocols response that ends the
// HTTP phase of the connection's life.
func writeHandshake(nc net.Conn, key string, timeout time.Duration) error {
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + ComputeAccept(key) + "\r\n\r\n"
	if err := nc.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("wshub: set handshake write deadline: %w", err)
	}
	if _, err := io.WriteString(nc, resp); err != nil {
		return fmt.Errorf("wshub: write handshake: %w", err)
	}
	return nil
}

// register adds an already-upgraded socket to the hub and starts its reader
// and writer goroutines. Split out from HandleUpgrade so tests can drive a
// registered connection over net.Pipe without an HTTP server.
func (h *Hub) register(nc net.Conn, br *bufio.Reader) *conn {
	c := &conn{
		hub:          h,
		nc:           nc,
		br:           br,
		send:         make(chan []byte, h.sendBuffer),
		control:      make(chan []byte, controlBuffer),
		done:         make(chan struct{}),
		writeTimeout: h.writeTimeout,
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.close()
		return c
	}
	h.conns[c] = struct{}{}
	h.mu.Unlock()

	go c.writeLoop()
	go c.readLoop()
	return c
}

// Broadcast pushes one typed message to every connected dashboard as
// {"type": msgType, "data": data}.
//
// It is safe to call from any number of goroutines and never blocks on the
// network: the frame is encoded once and handed to each connection's writer
// goroutine through a buffered channel. A client whose buffer is already full
// has fallen further behind than the hub will carry, and is closed with 1013
// rather than allowed to stall everyone else. Data that cannot be marshalled
// is a bug in the caller, not a reason to tear down sockets, so it is
// silently skipped.
func (h *Hub) Broadcast(msgType string, data any) {
	h.mu.RLock()
	if h.closed || len(h.conns) == 0 {
		h.mu.RUnlock()
		return // nobody watching, zero cost
	}
	targets := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	payload, err := json.Marshal(envelope{Type: msgType, Data: data})
	if err != nil {
		return
	}
	frame := encodeTextFrame(payload)

	// The snapshot above means the fan-out runs with no lock held, so one
	// slow connection cannot delay registration or teardown of another.
	for _, c := range targets {
		if !c.enqueue(frame) {
			c.gracefulClose(closeTryAgainLater, "client too slow")
		}
	}
}

// Count returns the number of dashboards currently connected.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// Close shuts the hub down: no further upgrades are accepted, and every live
// connection is detached immediately and sent a 1001 "going away" Close frame
// on the way out. It returns without waiting for the network; each socket is
// torn down once its Close frame is flushed, or after a short grace period if
// the peer is not reading. Close is idempotent.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	targets := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		targets = append(targets, c)
	}
	h.conns = make(map[*conn]struct{})
	h.mu.Unlock()

	for _, c := range targets {
		c.gracefulClose(closeGoingAway, "server shutting down")
	}
}

// remove detaches a connection from the registry. Idempotent, and safe to
// call from either of a connection's goroutines.
func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// conn is one dashboard socket. Exactly one goroutine (writeLoop) ever writes
// to nc, and exactly one (readLoop) ever reads from br, so the two never race
// even though bufio.Reader and net.Conn writes share the same file
// descriptor.
type conn struct {
	hub *Hub
	nc  net.Conn
	br  *bufio.Reader

	// send carries broadcast frames; a full send buffer means "drop me".
	send chan []byte
	// control carries pongs and close frames. Kept separate so a backlog of
	// data frames can never starve the protocol's own bookkeeping.
	control chan []byte
	// done is closed exactly once, when the socket is torn down.
	done chan struct{}

	writeTimeout time.Duration
	once         sync.Once
}

// enqueue hands a frame to the writer goroutine without ever blocking. It
// reports false when the buffer is full, which is the caller's signal to drop
// this client instead of waiting for it.
func (c *conn) enqueue(frame []byte) bool {
	select {
	case c.send <- frame:
		return true
	default:
		return false
	}
}

// enqueueControl offers a control frame to the writer. Control frames are
// best-effort: if even the small control buffer is full, this connection is
// already being torn down and one more pong changes nothing.
func (c *conn) enqueueControl(frame []byte) {
	select {
	case c.control <- frame:
	default:
	}
}

// close tears the socket down immediately and detaches it from the hub.
// Idempotent, and safe from any goroutine.
func (c *conn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.nc.Close()
	})
	c.hub.remove(c)
}

// gracefulClose detaches the connection at once (so no further broadcast
// touches it) and asks the writer goroutine to flush a Close frame. The
// socket dies as soon as that frame is written, or after closeGrace if the
// peer is not reading — which is the case that matters, because the whole
// point of dropping a slow client is not waiting on it.
func (c *conn) gracefulClose(code uint16, reason string) {
	select {
	case <-c.done:
		c.close() // already torn down; just make sure the hub forgot it
		return
	default:
	}
	c.hub.remove(c)
	c.enqueueControl(encodeCloseFrame(code, reason))
	time.AfterFunc(closeGrace, c.close)
}

// writeLoop is the ONE goroutine allowed to write to this socket. Every other
// goroutine reaches the wire by handing it a finished frame, which is what
// makes concurrent Broadcast calls safe: frames are serialised here, so their
// bytes can never interleave on the connection.
func (c *conn) writeLoop() {
	defer c.close()
	for {
		select {
		case <-c.done:
			return
		case frame := <-c.control:
			if err := c.writeFrame(frame); err != nil {
				return
			}
			if len(frame) > 0 && frame[0]&0x0f == opClose {
				return // we said goodbye; nothing more may go out
			}
		case frame := <-c.send:
			if err := c.writeFrame(frame); err != nil {
				return
			}
		}
	}
}

// writeFrame writes one complete frame under a deadline, so a half-open peer
// cannot wedge the writer goroutine forever.
func (c *conn) writeFrame(frame []byte) error {
	if err := c.nc.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return fmt.Errorf("wshub: set write deadline: %w", err)
	}
	if _, err := c.nc.Write(frame); err != nil {
		return fmt.Errorf("wshub: write frame: %w", err)
	}
	return nil
}

// readLoop parses inbound frames for as long as the socket lives. Dashboards
// do not send application data, but the connection still has to be READ:
// pings need pongs, a close needs honouring, and every frame has to be walked
// over properly to find where the next one begins. Any parse failure closes
// this one connection and returns — a hostile client can lose its own socket
// and nothing more.
func (c *conn) readLoop() {
	for {
		f, err := readFrame(c.br)
		if err != nil {
			switch {
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
				// The peer hung up, or we did. No point announcing anything.
				c.close()
			case errors.Is(err, ErrPayloadTooLarge):
				c.gracefulClose(closeMessageTooBig, "frame too large")
			default:
				c.gracefulClose(closeProtocolError, "malformed frame")
			}
			return
		}

		switch f.opcode {
		case opClose:
			// RFC 6455: answer a Close with a Close, then stop.
			c.gracefulClose(closeNormal, "")
			return
		case opPing:
			// A Pong must echo the Ping's payload verbatim.
			c.enqueueControl(encodeControlFrame(opPong, f.payload))
		case opPong, opText, opBinary, opContinuation:
			// Dashboards listen rather than talk. Anything they do say is
			// deliberately ignored — but it still had to be parsed, or the
			// next frame boundary would be lost.
		}
		// No default: readFrame has already rejected every opcode the RFC
		// reserves, so the six cases above are exhaustive.
	}
}

// frame is one decoded inbound frame with its payload already unmasked.
type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// readFrame reads exactly one client frame. It enforces the client-side rules
// of RFC 6455 section 5.2 — mask required, no reserved bits, control frames
// short and unfragmented — and refuses to allocate for a declared length it
// has not first checked. Every error is wrapped so callers can match the
// sentinels with errors.Is.
func readFrame(r *bufio.Reader) (frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return frame{}, fmt.Errorf("wshub: read frame header: %w", err)
	}

	f := frame{fin: hdr[0]&finBit != 0, opcode: hdr[0] & 0x0f}
	if hdr[0]&rsvBits != 0 {
		return frame{}, fmt.Errorf("wshub: read frame: %w", ErrReservedBits)
	}
	if hdr[1]&maskBit == 0 {
		return frame{}, fmt.Errorf("wshub: read frame: %w", ErrUnmaskedClientFrame)
	}

	// The 7-bit length, or one of its two escape values.
	length := uint64(hdr[1] &^ maskBit)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, fmt.Errorf("wshub: read 16-bit length: %w", err)
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, fmt.Errorf("wshub: read 64-bit length: %w", err)
		}
		length = binary.BigEndian.Uint64(ext[:])
		// The RFC forbids the high bit; it also happens to be the value that
		// would overflow an int64 length on the way to make().
		if length&(1<<63) != 0 {
			return frame{}, fmt.Errorf("wshub: read 64-bit length: %w", ErrPayloadTooLarge)
		}
	}

	if !knownOpcode(f.opcode) {
		return frame{}, fmt.Errorf("wshub: opcode 0x%X: %w", f.opcode, ErrUnknownOpcode)
	}
	if isControl(f.opcode) {
		if length > 125 {
			return frame{}, fmt.Errorf("wshub: opcode 0x%X: %w", f.opcode, ErrControlFrameTooLong)
		}
		if !f.fin {
			return frame{}, fmt.Errorf("wshub: opcode 0x%X: %w", f.opcode, ErrFragmentedControlFrame)
		}
	}
	if length > maxInboundPayload {
		return frame{}, fmt.Errorf("wshub: declared %d bytes: %w", length, ErrPayloadTooLarge)
	}

	var maskKey [4]byte
	if _, err := io.ReadFull(r, maskKey[:]); err != nil {
		return frame{}, fmt.Errorf("wshub: read masking key: %w", err)
	}
	if length > 0 {
		f.payload = make([]byte, length)
		if _, err := io.ReadFull(r, f.payload); err != nil {
			return frame{}, fmt.Errorf("wshub: read %d-byte payload: %w", length, err)
		}
		// Unmask in place: payload[i] ^= key[i % 4].
		for i := range f.payload {
			f.payload[i] ^= maskKey[i&3]
		}
	}
	return f, nil
}
