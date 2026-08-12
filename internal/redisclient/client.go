// Package redisclient is a minimal Redis client written against the RESP wire
// protocol with nothing but the standard library.
//
// The Node version imported the `redis` package; everything else in LimitPlane
// is built from scratch, so this keeps the Go build dependency-free and makes
// the protocol visible. RESP is genuinely small:
//
//	request   *<argc>\r\n $<len>\r\n<arg>\r\n ...      (an array of bulk strings)
//	replies   +OK\r\n            simple string
//	          -ERR message\r\n   error
//	          :42\r\n            integer
//	          $3\r\nabc\r\n      bulk string ($-1 = nil)
//	          *2\r\n...          array
//
// Connections are pooled: Redis is single-threaded per command but a pipeline
// of one connection would serialise every caller, so each in-flight command
// borrows its own connection and returns it.
package redisclient

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNil is returned for RESP nil bulk strings ($-1).
var ErrNil = errors.New("redis: nil reply")

type Client struct {
	addr     string
	username string
	password string
	db       int
	timeout  time.Duration

	mu   sync.Mutex
	idle []*conn
}

type conn struct {
	net net.Conn
	br  *bufio.Reader
}

// New dials nothing yet — connections are created lazily on first use so that
// a gateway configured with Redis still boots when Redis is briefly down.
// rawURL accepts redis://[user:pass@]host:port[/db].
func New(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis: bad url: %w", err)
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return nil, fmt.Errorf("redis: unsupported scheme %q", u.Scheme)
	}
	c := &Client{addr: u.Host, timeout: 3 * time.Second}
	if c.addr == "" {
		c.addr = "127.0.0.1:6379"
	}
	if !strings.Contains(c.addr, ":") {
		c.addr += ":6379"
	}
	if u.User != nil {
		c.username = u.User.Username()
		c.password, _ = u.User.Password()
	}
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			c.db = n
		}
	}
	return c, nil
}

func (c *Client) dial() (*conn, error) {
	nc, err := net.DialTimeout("tcp", c.addr, c.timeout)
	if err != nil {
		return nil, err
	}
	cn := &conn{net: nc, br: bufio.NewReader(nc)}

	// AUTH and SELECT ride on the connection, not the command, so they must be
	// replayed on every freshly-dialled socket.
	if c.password != "" {
		args := []string{"AUTH"}
		if c.username != "" {
			args = append(args, c.username)
		}
		args = append(args, c.password)
		if _, err := cn.do(c.timeout, args...); err != nil {
			nc.Close()
			return nil, err
		}
	}
	if c.db != 0 {
		if _, err := cn.do(c.timeout, "SELECT", strconv.Itoa(c.db)); err != nil {
			nc.Close()
			return nil, err
		}
	}
	return cn, nil
}

func (c *Client) get() (*conn, error) {
	c.mu.Lock()
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	return c.dial()
}

func (c *Client) put(cn *conn) {
	c.mu.Lock()
	// Cap the pool: a burst should not leave hundreds of sockets parked.
	if len(c.idle) < 16 {
		c.idle = append(c.idle, cn)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	cn.net.Close()
}

// Do runs one command and returns the decoded reply.
func (c *Client) Do(args ...string) (any, error) {
	cn, err := c.get()
	if err != nil {
		return nil, err
	}
	reply, err := cn.do(c.timeout, args...)
	if err != nil {
		// A broken pipe means this socket is unusable; drop it instead of
		// poisoning the pool for the next caller.
		cn.net.Close()
		return nil, err
	}
	c.put(cn)
	return reply, nil
}

// Int runs a command expecting an integer reply (INCR, EXPIRE, EVAL counters).
func (c *Client) Int(args ...string) (int64, error) {
	reply, err := c.Do(args...)
	if err != nil {
		return 0, err
	}
	switch v := reply.(type) {
	case int64:
		return v, nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("redis: expected integer, got %T", reply)
	}
}

// Ping is the health check the gateway uses at boot to decide whether the
// distributed limiter is actually usable.
func (c *Client) Ping() error {
	_, err := c.Do("PING")
	return err
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cn := range c.idle {
		cn.net.Close()
	}
	c.idle = nil
	return nil
}

// ---- wire encoding/decoding -------------------------------------------------

func (cn *conn) do(timeout time.Duration, args ...string) (any, error) {
	if err := cn.net.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := cn.net.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return readReply(cn.br)
}

func readReply(br *bufio.Reader) (any, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, errors.New("redis: short reply")
	}
	body := strings.TrimRight(line[1:], "\r\n")

	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, errors.New("redis: " + body)
	case ':':
		return strconv.ParseInt(body, 10, 64)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		buf := make([]byte, n+2) // payload + trailing CRLF
		if _, err := ioReadFull(br, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			item, err := readReply(br)
			if err != nil && !errors.Is(err, ErrNil) {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("redis: unknown reply type %q", line[0])
	}
}

func ioReadFull(br *bufio.Reader, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := br.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}
