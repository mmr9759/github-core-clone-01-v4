// Raw TCP HTTP/1.1 server: no net/http, no Date header, no per-request
// time calls. One goroutine per connection, sequential keep-alive requests,
// a single write per small response, TCP_NODELAY on every socket.
package main

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

type server struct {
	token string
	store *Store
	reqID uint64
}

func newServer(token string) *server {
	return &server{token: token, store: newStore()}
}

func (s *server) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go s.handleConn(conn)
	}
}

// ---------------------------------------------------------------------------
// request parsing

type header struct {
	name  string
	value string
}

type request struct {
	method string
	path   string // percent-decoded, no query
	query  string
	hdrs   []header
	body   []byte
	keep   bool
}

func (r *request) header(name string) (string, bool) {
	for i := range r.hdrs {
		if r.hdrs[i].name == name {
			return r.hdrs[i].value, true
		}
	}
	return "", false
}

// queryVal scans a raw query string for one key (percent-decoded value).
func queryVal(q, key string) (string, bool) {
	for len(q) > 0 {
		eq := strings.IndexByte(q, '=')
		amp := strings.IndexByte(q, '&')
		var pair string
		if amp < 0 {
			pair = q
			q = ""
		} else {
			pair = q[:amp]
			q = q[amp+1:]
		}
		if eq < 0 {
			continue
		}
		k := pair[:eq]
		if k == key {
			return pctDecode(pair[eq+1:]), true
		}
		_ = eq
	}
	return "", false
}

func pctDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, ok := hex2(s[i+1], s[i+2]); ok {
				b.WriteByte(v)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func hex2(a, b byte) (byte, bool) {
	hi, ok1 := hexVal(a)
	lo, ok2 := hexVal(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

const maxHeaderBytes = 64 << 10
const maxBodyBytes = 64 << 20

func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	rd := bufio.NewReaderSize(conn, 32768)
	var req request
	for {
		if !s.readRequest(conn, rd, &req) {
			return
		}
		resp := s.dispatch(&req)
		if !s.writeResponse(conn, resp, req.keep) {
			return
		}
		if !req.keep {
			return
		}
	}
}

func (s *server) readRequest(conn net.Conn, rd *bufio.Reader, req *request) bool {
	// Request line.
	line, err := readLine(rd, maxHeaderBytes)
	if err != nil {
		return false
	}
	if len(line) == 0 {
		// A stray blank line before a request (a client closing, or a
		// keep-alive probe): try once more, then give up.
		line, err = readLine(rd, maxHeaderBytes)
		if err != nil || len(line) == 0 {
			return false
		}
	}
	sp1 := strings.IndexByte(line, ' ')
	sp2 := strings.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return false
	}
	req.method = line[:sp1]
	target := line[sp1+1 : sp2]
	req.keep = true // HTTP/1.1 default

	// Split target into path and query.
	if i := strings.IndexByte(target, '?'); i >= 0 {
		req.path = pctDecode(target[:i])
		req.query = target[i+1:]
	} else {
		req.path = pctDecode(target)
		req.query = ""
	}

	// Headers.
	req.hdrs = req.hdrs[:0]
	total := 0
	var contentLength = -1
	chunked := false
	for {
		h, err := readLine(rd, maxHeaderBytes)
		if err != nil {
			return false
		}
		total += len(h) + 2
		if total > maxHeaderBytes {
			return false
		}
		if len(h) == 0 {
			break
		}
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimRight(h[:colon], " \t"))
		value := strings.TrimLeft(h[colon+1:], " \t")
		if len(req.hdrs) < 32 {
			req.hdrs = append(req.hdrs, header{name, value})
		}
		switch name {
		case "content-length":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				contentLength = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "connection":
			if strings.Contains(strings.ToLower(value), "close") {
				req.keep = false
			}
		case "expect":
			if strings.Contains(strings.ToLower(value), "100-continue") {
				conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
			}
		}
	}

	// Body.
	if chunked {
		body, ok := readChunked(rd)
		if !ok {
			return false
		}
		req.body = body
	} else if contentLength > 0 {
		if contentLength > maxBodyBytes {
			return false
		}
		buf := make([]byte, contentLength)
		if _, err := io.ReadFull(rd, buf); err != nil {
			return false
		}
		req.body = buf
	} else {
		req.body = nil
	}
	return true
}

func readChunked(rd *bufio.Reader) ([]byte, bool) {
	var out []byte
	for {
		line, err := readLine(rd, 128)
		if err != nil {
			return nil, false
		}
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, false
		}
		if n == 0 {
			// Trailer headers until a blank line.
			for {
				t, err := readLine(rd, maxHeaderBytes)
				if err != nil || len(t) == 0 {
					return out, err == nil
				}
			}
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(rd, chunk); err != nil {
			return nil, false
		}
		out = append(out, chunk...)
		if len(out) > maxBodyBytes {
			return nil, false
		}
		// Trailing CRLF after the chunk.
		if _, err := rd.ReadSlice('\n'); err != nil {
			return nil, false
		}
	}
}

// readLine reads one CRLF-terminated line without the terminator.
func readLine(rd *bufio.Reader, max int) (string, error) {
	line, err := rd.ReadSlice('\n')
	if err != nil && err != bufio.ErrBufferFull {
		return "", err
	}
	if len(line) > max {
		return "", io.ErrShortBuffer
	}
	// Strip trailing \r\n or \n.
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
		if n > 1 && line[n-2] == '\r' {
			line = line[:n-2]
		}
	}
	return string(line), nil
}

// ---------------------------------------------------------------------------
// responses

type response struct {
	status int
	ctype  string
	body   []byte
	extra  []header // extra headers, already formatted as name/value
}

var statusText = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func (s *server) writeResponse(conn net.Conn, resp response, keep bool) bool {
	id := atomic.AddUint64(&s.reqID, 1)
	text, ok := statusText[resp.status]
	if !ok {
		text = "Status"
	}
	var b []byte
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(resp.status), 10)
	b = append(b, ' ')
	b = append(b, text...)
	b = append(b, "\r\n"...)
	ctype := resp.ctype
	if ctype == "" {
		ctype = "application/json; charset=utf-8"
	}
	b = append(b, "Content-Type: "...)
	b = append(b, ctype...)
	b = append(b, "\r\n"...)
	b = append(b, "X-GitHub-Request-Id: r"...)

	b = strconv.AppendUint(b, id, 16)
	b = append(b, "\r\n"...)
	for i := range resp.extra {
		b = append(b, resp.extra[i].name...)
		b = append(b, ": "...)
		b = append(b, resp.extra[i].value...)
		b = append(b, "\r\n"...)
	}
	if !keep {
		b = append(b, "Connection: close\r\n"...)
	}
	b = append(b, "Content-Length: "...)
	b = strconv.AppendInt(b, int64(len(resp.body)), 10)
	b = append(b, "\r\n\r\n"...)
	if len(resp.body) == 0 {
		_, err := conn.Write(b)
		return err == nil
	}
	if len(b)+len(resp.body) <= 1<<17 {
		b = append(b, resp.body...)
		_, err := conn.Write(b)
		return err == nil
	}
	if _, err := conn.Write(b); err != nil {
		return false
	}
	_, err := conn.Write(resp.body)
	return err == nil
}

// ---------------------------------------------------------------------------
// small JSON helpers (hand-built, no encoding/json on hot paths)

func jsonErr(status int, msg string) response {
	return response{status: status, body: []byte(`{"message":"` + jsonEscape(msg) + `"}`)}
}

func jsonEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00`)
				b.WriteByte(hex[c>>4])
				b.WriteByte(hex[c&0xf])
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

func quote(s string) string { return `"` + jsonEscape(s) + `"` }

// authed reports whether the request carries this server's bearer token.
// When no token is configured (a local run without one), everything passes.
func (s *server) authed(r *request) bool {
	if s.token == "" {
		return true
	}
	v, ok := r.header("authorization")
	return ok && v == "Bearer "+s.token
}
