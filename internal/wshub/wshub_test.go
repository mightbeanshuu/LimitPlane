package wshub

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// testKey is the canonical Sec-WebSocket-Key from RFC 6455 section 1.3.
const testKey = "dGhlIHNhbXBsZSBub25jZQ=="

// newTestServer wires a hub to an httptest server on /ws.
func newTestServer(t *testing.T, h *Hub) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", h.HandleUpgrade)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// dialWS opens a raw TCP connection and performs the WebSocket handshake by
// hand — no client library involved, so the test exercises the real wire
// format rather than a helper's idea of it.
func dialWS(t *testing.T, srv *httptest.Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	nc, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	req := "GET /ws?token=letmein HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + testKey + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(nc, req); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), ComputeAccept(testKey); got != want {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	if got := strings.ToLower(resp.Header.Get("Upgrade")); got != "websocket" {
		t.Fatalf("Upgrade header = %q, want websocket", got)
	}
	return nc, br
}

// readServerFrame decodes one server -> client frame. Server frames must be
// unmasked, and this asserts that too.
func readServerFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[0]&finBit == 0 {
		return 0, nil, errors.New("server frame without FIN")
	}
	if hdr[1]&maskBit != 0 {
		return 0, nil, errors.New("server frame must not be masked")
	}
	opcode = hdr[0] & 0x0f
	n := uint64(hdr[1] &^ maskBit)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

// maskedClientFrame builds the frame a real browser would send: masked, with
// whichever length form the payload needs.
func maskedClientFrame(opcode byte, payload []byte) []byte {
	key := [4]byte{0xAA, 0x55, 0x0F, 0xF0}
	n := len(payload)

	var out []byte
	switch {
	case n < 126:
		out = []byte{finBit | opcode, maskBit | byte(n)}
	case n < 1<<16:
		out = []byte{finBit | opcode, maskBit | 126, byte(n >> 8), byte(n)}
	default:
		out = []byte{finBit | opcode, maskBit | 127, 0, 0, 0, 0,
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	out = append(out, key[:]...)
	for i := 0; i < n; i++ {
		out = append(out, payload[i]^key[i&3])
	}
	return out
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// upgradeRequest builds a well-formed upgrade request for the recorder tests.
func upgradeRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/ws?token=letmein", nil)
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Sec-WebSocket-Key", testKey)
	r.Header.Set("Sec-WebSocket-Version", "13")
	return r
}

// ---------------------------------------------------------------------------
// handshake maths
// ---------------------------------------------------------------------------

func TestComputeAccept(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			// The worked example in RFC 6455 section 1.3.
			name: "rfc 6455 canonical example",
			key:  "dGhlIHNhbXBsZSBub25jZQ==",
			want: "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=",
		},
		{
			// Not special-cased anywhere: an empty key still hashes.
			name: "empty key still hashes the guid",
			key:  "",
			want: "Kfh9QIsMVZcl6xEPYxPHzW8SZ8w=",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ComputeAccept(tc.key); got != tc.want {
				t.Errorf("ComputeAccept(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// outbound framing
// ---------------------------------------------------------------------------

func TestEncodeTextFrameShortForm(t *testing.T) {
	got := EncodeTextFrame("hi")
	want := []byte{0x81, 0x02, 'h', 'i'}
	if !bytes.Equal(got, want) {
		t.Fatalf("EncodeTextFrame(%q) = % x, want % x", "hi", got, want)
	}
}

func TestEncodeTextFrameBoundary125(t *testing.T) {
	// 125 is the largest payload that still fits the 7-bit length field.
	got := EncodeTextFrame(strings.Repeat("a", 125))
	if len(got) != 127 {
		t.Fatalf("frame length = %d, want 127", len(got))
	}
	if got[0] != 0x81 || got[1] != 125 {
		t.Fatalf("header = % x, want 81 7d", got[:2])
	}
}

func TestEncodeTextFrame16BitForm(t *testing.T) {
	// 126 is the first length that needs the 16-bit escape.
	for _, n := range []int{126, 300, 65535} {
		frame := EncodeTextFrame(strings.Repeat("a", n))
		if frame[0] != 0x81 {
			t.Fatalf("n=%d: byte0 = %#x, want 0x81", n, frame[0])
		}
		if frame[1] != 126 {
			t.Fatalf("n=%d: length byte = %d, want 126 (16-bit escape)", n, frame[1])
		}
		if got := binary.BigEndian.Uint16(frame[2:4]); int(got) != n {
			t.Fatalf("n=%d: extended length = %d", n, got)
		}
		if len(frame) != 4+n {
			t.Fatalf("n=%d: frame length = %d, want %d", n, len(frame), 4+n)
		}
		if !bytes.Equal(frame[4:], bytes.Repeat([]byte("a"), n)) {
			t.Fatalf("n=%d: payload corrupted", n)
		}
	}
}

func TestEncodeTextFrame64BitForm(t *testing.T) {
	// 65536 is the first length that needs the 64-bit escape — the header can
	// be asserted exactly without allocating anything dramatic.
	const n = 1 << 16
	frame := EncodeTextFrame(strings.Repeat("a", n))

	wantHeader := []byte{0x81, 127, 0, 0, 0, 0, 0, 1, 0, 0}
	if !bytes.Equal(frame[:10], wantHeader) {
		t.Fatalf("header = % x, want % x", frame[:10], wantHeader)
	}
	if got := binary.BigEndian.Uint64(frame[2:10]); got != n {
		t.Fatalf("extended length = %d, want %d", got, n)
	}
	if len(frame) != 10+n {
		t.Fatalf("frame length = %d, want %d", len(frame), 10+n)
	}
}

// ---------------------------------------------------------------------------
// handshake rejection — none of these may hijack
// ---------------------------------------------------------------------------

// httptest.ResponseRecorder deliberately does NOT implement http.Hijacker, so
// any code path that reaches the hijack answers 500. That is what makes the
// rejection tests below meaningful: a 401 or 400 proves we returned first.
func TestRecorderCannotHijack(t *testing.T) {
	h := New(nil)
	defer h.Close()

	rec := httptest.NewRecorder()
	h.HandleUpgrade(rec, upgradeRequest())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (recorder is not a Hijacker)", rec.Code)
	}
}

func TestHandleUpgradeUnauthorized(t *testing.T) {
	var sawRequest bool
	h := New(func(r *http.Request) bool {
		sawRequest = true
		return r.URL.Query().Get("token") == "good"
	})
	defer h.Close()

	rec := httptest.NewRecorder()
	h.HandleUpgrade(rec, upgradeRequest()) // token=letmein, not "good"

	if !sawRequest {
		t.Fatal("authorize was never consulted")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", h.Count())
	}
}

func TestHandleUpgradeRejectsBadHandshakes(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*http.Request)
		wantMsg string
	}{
		{
			name:    "missing key",
			mutate:  func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") },
			wantMsg: "Sec-WebSocket-Key",
		},
		{
			name:    "blank key",
			mutate:  func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "   ") },
			wantMsg: "Sec-WebSocket-Key",
		},
		{
			name:    "old version",
			mutate:  func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "8") },
			wantMsg: "Sec-WebSocket-Version",
		},
		{
			name:    "missing version",
			mutate:  func(r *http.Request) { r.Header.Del("Sec-WebSocket-Version") },
			wantMsg: "Sec-WebSocket-Version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := New(nil)
			defer h.Close()

			r := upgradeRequest()
			tc.mutate(r)
			rec := httptest.NewRecorder()
			h.HandleUpgrade(rec, r)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantMsg) {
				t.Errorf("body = %q, want mention of %q", rec.Body.String(), tc.wantMsg)
			}
			if h.Count() != 0 {
				t.Errorf("Count() = %d, want 0", h.Count())
			}
		})
	}
}

func TestHandleUpgradeAfterCloseIsUnavailable(t *testing.T) {
	h := New(nil)
	h.Close()

	rec := httptest.NewRecorder()
	h.HandleUpgrade(rec, upgradeRequest())

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// end to end
// ---------------------------------------------------------------------------

func TestBroadcastEndToEnd(t *testing.T) {
	h := New(func(r *http.Request) bool { return r.URL.Query().Get("token") == "letmein" })
	defer h.Close()
	srv := newTestServer(t, h)

	_, br := dialWS(t, srv)
	waitFor(t, "the socket to register", func() bool { return h.Count() == 1 })

	h.Broadcast("stats", map[string]any{"allowed": 3, "blocked": 1})

	opcode, payload, err := readServerFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if opcode != opText {
		t.Fatalf("opcode = %#x, want 0x1 (text)", opcode)
	}

	// The exact bytes on the wire, not just something that parses.
	const want = `{"type":"stats","data":{"allowed":3,"blocked":1}}`
	if string(payload) != want {
		t.Fatalf("payload = %s, want %s", payload, want)
	}

	var env struct {
		Type string         `json:"type"`
		Data map[string]int `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if env.Type != "stats" || env.Data["allowed"] != 3 || env.Data["blocked"] != 1 {
		t.Fatalf("decoded envelope = %+v", env)
	}
}

func TestBroadcastWithNoClientsIsANoop(t *testing.T) {
	h := New(nil)
	defer h.Close()
	h.Broadcast("stats", map[string]any{"allowed": 1}) // must not panic
	if h.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", h.Count())
	}
}

func TestBroadcastSkipsUnmarshalableData(t *testing.T) {
	h := New(nil)
	defer h.Close()
	srv := newTestServer(t, h)

	_, br := dialWS(t, srv)
	waitFor(t, "the socket to register", func() bool { return h.Count() == 1 })

	h.Broadcast("bad", make(chan int)) // channels cannot be marshalled
	h.Broadcast("good", "ok")

	// The bad broadcast is dropped, the connection survives, and the next
	// message is the good one.
	opcode, payload, err := readServerFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if opcode != opText || string(payload) != `{"type":"good","data":"ok"}` {
		t.Fatalf("opcode %#x payload %s", opcode, payload)
	}
	if h.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", h.Count())
	}
}

func TestMaskedClientFramesAreSkippedAndPingIsAnswered(t *testing.T) {
	h := New(nil)
	defer h.Close()
	srv := newTestServer(t, h)

	nc, br := dialWS(t, srv)
	waitFor(t, "the socket to register", func() bool { return h.Count() == 1 })

	// Three inbound frames the hub has nothing to say about, one per length
	// form. Getting the pong back proves each masked payload was walked over
	// exactly, because a single byte of drift would desynchronise the parser.
	var out []byte
	out = append(out, maskedClientFrame(opText, []byte("chatter"))...)                   // 7-bit
	out = append(out, maskedClientFrame(opText, bytes.Repeat([]byte("x"), 300))...)      // 16-bit
	out = append(out, maskedClientFrame(opBinary, bytes.Repeat([]byte{0xEE}, 1<<16))...) // 64-bit
	out = append(out, maskedClientFrame(opPing, []byte("are you there"))...)             // then a ping
	if _, err := nc.Write(out); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	opcode, payload, err := readServerFrame(br)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong {
		t.Fatalf("opcode = %#x, want 0xA (pong)", opcode)
	}
	if string(payload) != "are you there" {
		t.Fatalf("pong payload = %q, want the ping payload echoed", payload)
	}
	if h.Count() != 1 {
		t.Fatalf("Count() = %d, want the connection still alive", h.Count())
	}
}

func TestClientCloseFrameIsEchoedAndDetached(t *testing.T) {
	h := New(nil)
	defer h.Close()
	srv := newTestServer(t, h)

	nc, br := dialWS(t, srv)
	waitFor(t, "the socket to register", func() bool { return h.Count() == 1 })

	if _, err := nc.Write(maskedClientFrame(opClose, []byte{0x03, 0xE8})); err != nil { // 1000
		t.Fatalf("write close: %v", err)
	}

	opcode, payload, err := readServerFrame(br)
	if err != nil {
		t.Fatalf("read close echo: %v", err)
	}
	if opcode != opClose {
		t.Fatalf("opcode = %#x, want 0x8 (close)", opcode)
	}
	if len(payload) < 2 || binary.BigEndian.Uint16(payload[:2]) != closeNormal {
		t.Fatalf("close payload = % x, want a 1000 status code", payload)
	}
	waitFor(t, "the socket to be forgotten", func() bool { return h.Count() == 0 })
}

func TestMalformedFrameClosesOnlyThatConnection(t *testing.T) {
	h := New(nil)
	defer h.Close()
	srv := newTestServer(t, h)

	victimConn, _ := dialWS(t, srv)
	_, survivorBR := dialWS(t, srv)
	waitFor(t, "two sockets", func() bool { return h.Count() == 2 })

	// Unmasked client frame: illegal, and a real client never sends one.
	if _, err := victimConn.Write([]byte{finBit | opText, 0x02, 'h', 'i'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, "the hostile socket to be dropped", func() bool { return h.Count() == 1 })

	// The other dashboard is untouched and still receiving.
	h.Broadcast("decision", "still here")
	opcode, payload, err := readServerFrame(survivorBR)
	if err != nil {
		t.Fatalf("survivor read: %v", err)
	}
	if opcode != opText || string(payload) != `{"type":"decision","data":"still here"}` {
		t.Fatalf("survivor got opcode %#x payload %s", opcode, payload)
	}
}

func TestHubCloseSaysGoingAway(t *testing.T) {
	h := New(nil)
	srv := newTestServer(t, h)

	_, br := dialWS(t, srv)
	waitFor(t, "the socket to register", func() bool { return h.Count() == 1 })

	h.Close()

	if h.Count() != 0 {
		t.Fatalf("Count() = %d immediately after Close, want 0", h.Count())
	}
	opcode, payload, err := readServerFrame(br)
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	if opcode != opClose {
		t.Fatalf("opcode = %#x, want 0x8 (close)", opcode)
	}
	if len(payload) < 2 || binary.BigEndian.Uint16(payload[:2]) != closeGoingAway {
		t.Fatalf("close payload = % x, want status 1001", payload)
	}
	h.Close() // idempotent
}

// ---------------------------------------------------------------------------
// the two problems the Node version got away with
// ---------------------------------------------------------------------------

// A client that never reads must be dropped, not waited on. net.Pipe is the
// clean way to prove it: it is fully synchronous, so a peer that does not read
// blocks the writer on the very first frame with no kernel buffer to hide
// behind. The send buffer then fills, and the hub must let the client go.
func TestBroadcastDropsSlowClient(t *testing.T) {
	h := New(nil)
	h.sendBuffer = 1 // the same rule as the default, reached sooner
	defer h.Close()

	serverSide, clientSide := net.Pipe()
	defer clientSide.Close() // deliberately never read from
	h.register(serverSide, bufio.NewReader(serverSide))

	if h.Count() != 1 {
		t.Fatalf("Count() = %d, want 1", h.Count())
	}

	start := time.Now()
	for i := 0; i < 10; i++ {
		h.Broadcast("stats", i)
	}
	elapsed := time.Since(start)

	// The point of the exercise: the broadcaster never waited on the network.
	if elapsed > time.Second {
		t.Fatalf("Broadcast blocked for %v on a client that never reads", elapsed)
	}
	if h.Count() != 0 {
		t.Fatalf("Count() = %d, want the wedged client dropped", h.Count())
	}
}

// Broadcast is called from a stats ticker, request handlers and the AI review
// loop at once. If two goroutines could write to one socket, their bytes would
// interleave and every client would see corrupt frames. Each client here
// checks that every frame it receives is whole and parses as JSON — with
// -race, this covers both halves of the problem.
func TestConcurrentBroadcastsProduceWholeFrames(t *testing.T) {
	const (
		clients          = 4
		writers          = 6
		perWriter        = 25
		expectedMessages = writers * perWriter
	)

	h := New(nil)
	h.sendBuffer = 4096 // no drops in this test; we are measuring interleaving
	defer h.Close()
	srv := newTestServer(t, h)

	readers := make([]*bufio.Reader, clients)
	for i := range readers {
		_, br := dialWS(t, srv)
		readers[i] = br
	}
	waitFor(t, "every socket to register", func() bool { return h.Count() == clients })

	// Every client drains concurrently while the writers hammer the hub.
	var readWG sync.WaitGroup
	errCh := make(chan error, clients)
	for i, br := range readers {
		readWG.Add(1)
		go func(i int, br *bufio.Reader) {
			defer readWG.Done()
			for n := 0; n < expectedMessages; n++ {
				opcode, payload, err := readServerFrame(br)
				if err != nil {
					errCh <- fmt.Errorf("client %d frame %d: %w", i, n, err)
					return
				}
				if opcode != opText {
					errCh <- fmt.Errorf("client %d frame %d: opcode %#x", i, n, opcode)
					return
				}
				var env struct {
					Type string `json:"type"`
					Data int    `json:"data"`
				}
				if err := json.Unmarshal(payload, &env); err != nil {
					errCh <- fmt.Errorf("client %d frame %d: interleaved frame %q: %w", i, n, payload, err)
					return
				}
				if env.Type != "decision" {
					errCh <- fmt.Errorf("client %d frame %d: type %q", i, n, env.Type)
					return
				}
			}
		}(i, br)
	}

	var writeWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writeWG.Add(1)
		go func(w int) {
			defer writeWG.Done()
			for n := 0; n < perWriter; n++ {
				h.Broadcast("decision", w*1000+n)
			}
		}(w)
	}

	// Count() and Close() race against the broadcasters on purpose.
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		for n := 0; n < 200; n++ {
			_ = h.Count()
		}
	}()

	writeWG.Wait()
	readWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if h.Count() != clients {
		t.Fatalf("Count() = %d, want %d — a client was dropped", h.Count(), clients)
	}
}

// ---------------------------------------------------------------------------
// inbound frame parsing
// ---------------------------------------------------------------------------

func TestReadFrameUnmasksPayload(t *testing.T) {
	tests := []struct {
		name    string
		opcode  byte
		payload []byte
	}{
		{"empty", opPing, nil},
		{"short form", opText, []byte("hello")},
		{"16-bit form", opText, bytes.Repeat([]byte("z"), 1000)},
		{"64-bit form", opBinary, bytes.Repeat([]byte{0x7F}, 1<<16)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A second frame follows, so a length mistake shows up as a
			// mis-parsed trailer rather than passing silently.
			wire := append(maskedClientFrame(tc.opcode, tc.payload), maskedClientFrame(opPing, []byte("next"))...)
			br := bufio.NewReader(bytes.NewReader(wire))

			f, err := readFrame(br)
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if f.opcode != tc.opcode {
				t.Fatalf("opcode = %#x, want %#x", f.opcode, tc.opcode)
			}
			if !f.fin {
				t.Fatal("fin = false, want true")
			}
			if !bytes.Equal(f.payload, tc.payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(f.payload), len(tc.payload))
			}

			trailer, err := readFrame(br)
			if err != nil {
				t.Fatalf("readFrame(trailer): %v", err)
			}
			if trailer.opcode != opPing || string(trailer.payload) != "next" {
				t.Fatalf("trailer = opcode %#x payload %q — frame boundary lost", trailer.opcode, trailer.payload)
			}
		})
	}
}

func TestReadFrameRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		wire  []byte
		want  error
		anyOf []error
	}{
		{
			name: "unmasked client frame",
			wire: []byte{finBit | opText, 0x02, 'h', 'i'},
			want: ErrUnmaskedClientFrame,
		},
		{
			name: "reserved bits set",
			wire: []byte{finBit | 0x40 | opText, maskBit | 0x00, 0, 0, 0, 0},
			want: ErrReservedBits,
		},
		{
			name: "reserved opcode",
			wire: []byte{finBit | 0x03, maskBit | 0x00, 0, 0, 0, 0},
			want: ErrUnknownOpcode,
		},
		{
			name: "control frame over 125 bytes",
			wire: []byte{finBit | opPing, maskBit | 126, 0x00, 0xC8},
			want: ErrControlFrameTooLong,
		},
		{
			name: "fragmented control frame",
			wire: []byte{opPing, maskBit | 0x00},
			want: ErrFragmentedControlFrame,
		},
		{
			name: "64-bit length with the high bit set",
			wire: []byte{finBit | opBinary, maskBit | 127, 0x80, 0, 0, 0, 0, 0, 0, 0},
			want: ErrPayloadTooLarge,
		},
		{
			name: "payload beyond the inbound cap",
			wire: []byte{finBit | opBinary, maskBit | 127, 0, 0, 0, 0, 0x00, 0x20, 0x00, 0x01},
			want: ErrPayloadTooLarge,
		},
		{
			name:  "no bytes at all",
			wire:  nil,
			anyOf: []error{io.EOF},
		},
		{
			name:  "truncated header",
			wire:  []byte{finBit | opText},
			anyOf: []error{io.ErrUnexpectedEOF},
		},
		{
			name:  "truncated masking key",
			wire:  []byte{finBit | opText, maskBit | 0x04, 0xAA, 0x55},
			anyOf: []error{io.ErrUnexpectedEOF},
		},
		{
			name:  "truncated payload",
			wire:  []byte{finBit | opText, maskBit | 0x04, 0xAA, 0x55, 0x0F, 0xF0, 0x01, 0x02},
			anyOf: []error{io.ErrUnexpectedEOF},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readFrame(bufio.NewReader(bytes.NewReader(tc.wire)))
			if err == nil {
				t.Fatal("readFrame accepted malformed input")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			for _, want := range tc.anyOf {
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want %v", err, want)
				}
			}
			// Wrapped, never bare: the message says which stage failed.
			if !strings.HasPrefix(err.Error(), "wshub: ") {
				t.Errorf("err = %q, want a wshub-prefixed message", err)
			}
		})
	}
}

// An oversized frame must be refused from its DECLARED length, before any
// allocation — otherwise two bytes of header buy a hostile client a
// multi-gigabyte allocation.
func TestReadFrameDoesNotAllocateForDeclaredLength(t *testing.T) {
	// Declares 2^62 bytes and supplies none of them.
	wire := []byte{finBit | opBinary, maskBit | 127, 0x40, 0, 0, 0, 0, 0, 0, 0}
	done := make(chan error, 1)
	go func() {
		_, err := readFrame(bufio.NewReader(bytes.NewReader(wire)))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readFrame is trying to honour the declared length")
	}
}
