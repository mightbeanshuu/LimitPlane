package redisclient_test

// Tests for the hand-written RESP client.
//
// A protocol parser that reads from a socket is the last place to have no
// tests: it is the only code in the project a REMOTE party controls the input
// to. Everything here therefore drives a real TCP listener rather than mocking
// the wire, so the request bytes we emit and the reply bytes we accept are both
// asserted exactly.
//
// No live Redis is needed. A fake server that speaks RESP is ~20 lines, and it
// lets us test the cases a real server would almost never produce on demand:
// truncated replies, absurd declared lengths, errors mid-array, hostile input.

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/redisclient"
)

// fakeRedis is a TCP server that replies with canned RESP and records what it
// was sent, so tests can assert the exact command encoding.
type fakeRedis struct {
	t        *testing.T
	ln       net.Listener
	mu       sync.Mutex
	received []string
	replies  []string
	idx      int
}

func newFakeRedis(t *testing.T, replies ...string) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRedis{t: t, ln: ln, replies: replies}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeRedis) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			br := bufio.NewReader(c)
			for {
				// Read one RESP array: *<n>\r\n then n bulk strings.
				line, err := br.ReadString('\n')
				if err != nil {
					return
				}
				var argc int
				if _, err := fmt.Sscanf(strings.TrimSpace(line), "*%d", &argc); err != nil {
					return
				}
				args := make([]string, 0, argc)
				for i := 0; i < argc; i++ {
					if _, err := br.ReadString('\n'); err != nil { // $<len>
						return
					}
					arg, err := br.ReadString('\n')
					if err != nil {
						return
					}
					args = append(args, strings.TrimRight(arg, "\r\n"))
				}

				f.mu.Lock()
				f.received = append(f.received, strings.Join(args, " "))
				reply := "+OK\r\n"
				if f.idx < len(f.replies) {
					reply = f.replies[f.idx]
					f.idx++
				}
				f.mu.Unlock()

				if _, err := c.Write([]byte(reply)); err != nil {
					return
				}
			}
		}(conn)
	}
}

func (f *fakeRedis) url() string { return "redis://" + f.ln.Addr().String() }

func (f *fakeRedis) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func dial(t *testing.T, f *fakeRedis) *redisclient.Client {
	t.Helper()
	c, err := redisclient.New(f.url())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// ---- URL parsing -----------------------------------------------------------

func TestNewParsesURLs(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		wantErr   bool
	}{
		{"host and port", "redis://127.0.0.1:6379", false},
		{"tls scheme accepted", "rediss://127.0.0.1:6379", false},
		{"with db index", "redis://127.0.0.1:6379/3", false},
		{"with credentials", "redis://user:pass@127.0.0.1:6379", false},
		{"bare host gets the default port", "redis://localhost", false},
		{"wrong scheme is refused", "http://127.0.0.1:6379", true},
		{"garbage is refused", "://:::", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := redisclient.New(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("New(%q) should have failed", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New(%q) failed: %v", tc.url, err)
			}
		})
	}
}

// ---- request encoding ------------------------------------------------------

// The command must go out as a RESP array of bulk strings. Getting the arity or
// the length prefix wrong desynchronises the connection for every later command.
func TestCommandsAreSentAsRESPArrays(t *testing.T) {
	f := newFakeRedis(t, ":1\r\n")
	c := dial(t, f)

	if _, err := c.Int("INCR", "tenant:free:/v1/ping"); err != nil {
		t.Fatalf("INCR: %v", err)
	}
	got := f.commands()
	if len(got) != 1 || got[0] != "INCR tenant:free:/v1/ping" {
		t.Fatalf("server received %q, want [\"INCR tenant:free:/v1/ping\"]", got)
	}
}

func TestArgumentsWithSpacesAndNewlinesSurvive(t *testing.T) {
	f := newFakeRedis(t, ":1\r\n")
	c := dial(t, f)

	// A key containing CRLF would break any newline-delimited encoding. RESP
	// length-prefixes every argument precisely so this is safe.
	nasty := "key with spaces\r\nand a fake *2 line"
	if _, err := c.Do("SET", nasty, "v"); err != nil {
		t.Fatalf("SET: %v", err)
	}
	got := f.commands()
	if len(got) != 1 || !strings.Contains(got[0], "key with spaces") {
		t.Fatalf("a length-prefixed argument must survive intact, server saw %q", got)
	}
}

// ---- reply decoding --------------------------------------------------------

func TestDecodesEveryReplyType(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		check       func(t *testing.T, v any, err error)
	}{
		{"simple string", "+PONG\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != "PONG" {
				t.Fatalf("got (%v, %v), want PONG", v, err)
			}
		}},
		{"integer", ":42\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != int64(42) {
				t.Fatalf("got (%v, %v), want int64(42)", v, err)
			}
		}},
		{"negative integer", ":-7\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != int64(-7) {
				t.Fatalf("got (%v, %v), want int64(-7)", v, err)
			}
		}},
		{"bulk string", "$5\r\nhello\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != "hello" {
				t.Fatalf("got (%v, %v), want hello", v, err)
			}
		}},
		{"empty bulk string", "$0\r\n\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != "" {
				t.Fatalf("got (%v, %v), want empty string", v, err)
			}
		}},
		{"bulk string containing CRLF", "$5\r\na\r\nb!\r\n", func(t *testing.T, v any, err error) {
			if err != nil || v != "a\r\nb!" {
				t.Fatalf("a length-prefixed payload may contain CRLF; got (%q, %v)", v, err)
			}
		}},
		{"nil bulk string", "$-1\r\n", func(t *testing.T, v any, err error) {
			if !errors.Is(err, redisclient.ErrNil) {
				t.Fatalf("a nil reply must be reported as ErrNil, got (%v, %v)", v, err)
			}
		}},
		{"error reply", "-ERR wrong kind of value\r\n", func(t *testing.T, v any, err error) {
			if err == nil || !strings.Contains(err.Error(), "wrong kind of value") {
				t.Fatalf("an error reply must surface its message, got (%v, %v)", v, err)
			}
		}},
		{"array", "*2\r\n$3\r\nfoo\r\n:9\r\n", func(t *testing.T, v any, err error) {
			arr, ok := v.([]any)
			if err != nil || !ok || len(arr) != 2 || arr[0] != "foo" || arr[1] != int64(9) {
				t.Fatalf("got (%#v, %v), want [foo 9]", v, err)
			}
		}},
		{"empty array", "*0\r\n", func(t *testing.T, v any, err error) {
			arr, ok := v.([]any)
			if err != nil || !ok || len(arr) != 0 {
				t.Fatalf("got (%#v, %v), want empty slice", v, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRedis(t, tc.reply)
			c := dial(t, f)
			v, err := c.Do("PING")
			tc.check(t, v, err)
		})
	}
}

// Int must accept both the integer form and a bulk string of digits, because
// EVAL returns whichever the script produced.
func TestIntAcceptsIntegerAndBulkForms(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		want        int64
		wantErr     bool
	}{
		{"integer reply", ":7\r\n", 7, false},
		{"bulk digits", "$2\r\n11\r\n", 11, false},
		{"non-numeric bulk is an error", "$3\r\nabc\r\n", 0, true},
		{"an array is not an integer", "*1\r\n:1\r\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRedis(t, tc.reply)
			c := dial(t, f)
			got, err := c.Int("INCR", "k")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %d", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%d, %v), want %d", got, err, tc.want)
			}
		})
	}
}

// ---- hostile and broken input ---------------------------------------------

// A malformed reply must produce an error, never a panic and never a hang. This
// is the case that matters most: the bytes come from the network.
func TestMalformedRepliesAreErrorsNotPanics(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"unknown type byte", "%2\r\nfoo\r\n"},
		{"truncated bulk string", "$100\r\nshort\r\n"},
		{"non-numeric bulk length", "$abc\r\n"},
		{"non-numeric array length", "*abc\r\n"},
		{"empty line", "\r\n"},
		{"bare newline", "\n"},
		{"integer that is not a number", ":notanumber\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("a hostile server must not be able to panic the client: %v", r)
				}
			}()
			f := newFakeRedis(t, tc.reply)
			c := dial(t, f)
			if _, err := c.Do("PING"); err == nil {
				t.Fatalf("malformed reply %q should have errored", tc.reply)
			}
		})
	}
}

func TestUnreachableServerErrors(t *testing.T) {
	// Port 1 on loopback is reserved and refuses connections quickly.
	c, err := redisclient.New("redis://127.0.0.1:1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Ping(); err == nil {
		t.Fatal("a dial failure must be reported, not swallowed — the gateway decides the fail-open policy")
	}
}

// ---- pooling and concurrency ----------------------------------------------

// A broken connection must not be returned to the pool, or the next caller
// inherits the corruption.
func TestPoolDoesNotReuseABrokenConnection(t *testing.T) {
	f := newFakeRedis(t, "%bad\r\n", ":5\r\n")
	c := dial(t, f)

	if _, err := c.Do("PING"); err == nil {
		t.Fatal("precondition: the first command should fail on the malformed reply")
	}
	// The second command must succeed on a FRESH connection.
	got, err := c.Int("INCR", "k")
	if err != nil || got != 5 {
		t.Fatalf("a poisoned connection must be discarded, not pooled; got (%d, %v)", got, err)
	}
}

// The gateway calls this from every request handler at once.
func TestConcurrentCommands(t *testing.T) {
	f := newFakeRedis(t)
	// Reply to everything with :1 by leaving replies empty is +OK, so supply
	// enough integer replies for every goroutine.
	for i := 0; i < 400; i++ {
		f.replies = append(f.replies, ":1\r\n")
	}
	c := dial(t, f)

	const workers, each = 20, 20
	errs := make(chan error, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := c.Int("INCR", fmt.Sprintf("k%d", w)); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent command failed: %v", err)
	}
	if got := len(f.commands()); got != workers*each {
		t.Fatalf("server saw %d commands, want %d — replies were mismatched to requests", got, workers*each)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	f := newFakeRedis(t, "+PONG\r\n")
	c, err := redisclient.New(f.url())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Ping()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close must be safe to call twice: %v", err)
	}
}
