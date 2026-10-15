package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// dialAddr defaults the port for scheme-less-host URLs (Socket Mode URLs
// have none).
func dialAddr(u *url.URL) string {
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	return host
}

// dialWS opens a minimal RFC6455 client websocket (text frames, no
// dependencies). Slack Socket Mode sends sequential envelopes; the
// handshake plus masked client frames are all that's needed.
func dialWS(ctx context.Context, rawURL string) (wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := dialAddr(u)
	path := u.RequestURI()
	// tls/net.Dial require host:PORT; Socket Mode URLs carry no port
	// (wss-primary.slack.com) — default it or every dial dies with
	// "missing port in address" (found live: the relay never connected,
	// on any deploy).
	// wss:// endpoints require TLS — a plaintext dial would leak the
	// handshake and fail (review finding 2).
	var conn net.Conn
	if u.Scheme == "wss" {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", host, nil)
	} else {
		conn, err = (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	key := wsKey()
	req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "101") {
		conn.Close()
		limit := n
		if limit > 200 {
			limit = 200
		}
		return nil, fmt.Errorf("ws handshake failed: %d %q", n, string(buf[:limit]))
	}
	_ = conn.SetDeadline(time.Time{})
	return &ws{c: conn}, nil
}

func wsKey() string {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(i*7 + 13)
	}
	return base64enc(b)
}

func base64enc(b []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out = append(out, tbl[(n>>18)&63], tbl[(n>>12)&63])
		if rem > 1 {
			out = append(out, tbl[(n>>6)&63])
		} else {
			out = append(out, '=')
		}
		if rem > 2 {
			out = append(out, tbl[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// ws is a text-frame websocket conn.
type ws struct {
	c  net.Conn
	mu sync.Mutex
}

func (w *ws) Read(ctx context.Context) ([]byte, error) {
	// frame header: fin/opcode, mask/len, (extended len)
	hdr := make([]byte, 2)
	if _, err := ioReadFull(w.c, hdr); err != nil {
		return nil, err
	}
	opcode := hdr[0] & 0x0f
	length := int(hdr[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err := ioReadFull(w.c, ext); err != nil {
			return nil, err
		}
		length = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err := ioReadFull(w.c, ext); err != nil {
			return nil, err
		}
		length = 0
		for _, b := range ext {
			length = length<<8 | int(b)
		}
	}
	payload := make([]byte, length)
	if _, err := ioReadFull(w.c, payload); err != nil {
		return nil, err
	}
	if opcode == 8 { // close
		return nil, errors.New("ws closed")
	}
	if opcode == 9 { // ping -> pong
		_ = w.Write(ctx, append([]byte{0x8a, byte(len(payload))}, payload...))
		return nil, nil
	}
	return payload, nil
}

func (w *ws) Write(ctx context.Context, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// client frames must be masked
	var mask [4]byte
	mask[0], mask[1], mask[2], mask[3] = 0x37, 0xfa, 0x21, 0x3d
	masked := make([]byte, len(data))
	for i, b := range data {
		masked[i] = b ^ mask[i%4]
	}
	var hdr []byte
	switch {
	case len(data) < 126:
		hdr = []byte{0x81, byte(0x80 | len(data))}
	case len(data) < 65536:
		hdr = []byte{0x81, 0x80 | 126, byte(len(data) >> 8), byte(len(data))}
	default:
		hdr = []byte{0x81, 0x80 | 127}
		l := len(data)
		for i := 7; i >= 0; i-- {
			hdr = append(hdr, byte(l>>(8*i)))
		}
	}
	_ = hdr
	_, err := w.c.Write(append(append(hdr, mask[:]...), masked...))
	return err
}

func (w *ws) Close() { _ = w.c.Close() }

func ioReadFull(c net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := c.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
