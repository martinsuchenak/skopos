package notify

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RFC 6455 opcodes.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xa
)

const (
	// maxMessage caps one frame and one reassembled message. Socket Mode
	// envelopes are a few KB; anything near this is a broken or hostile
	// peer, and the length must be rejected BEFORE it reaches make (a
	// 64-bit extended length went straight to make — review M4).
	maxMessage = 16 << 20
	// readTimeout bounds the wait for any frame. The keepalive pings every
	// pingInterval and the server's pong (or Slack's own pings) resets it,
	// so only a half-open socket — sleep, NAT timeout — ever hits it.
	readTimeout  = 90 * time.Second
	pingInterval = 30 * time.Second
	writeTimeout = 10 * time.Second
	dialTimeout  = 15 * time.Second
)

// wsGUID is the RFC 6455 handshake constant (Sec-WebSocket-Accept).
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// dialAddr defaults the port for scheme-less-host URLs (Socket Mode URLs
// have none).
func dialAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := "80"
	if u.Scheme == "wss" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// dialWS opens a minimal RFC 6455 client websocket (no dependencies).
// Slack Socket Mode sends sequential text envelopes; the client side is the
// handshake, masked frames, control-frame handling and a keepalive.
func dialWS(ctx context.Context, rawURL string) (wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	// tls/net.Dial require host:PORT; Socket Mode URLs carry no port
	// (wss-primary.slack.com) — default it or every dial dies with
	// "missing port in address" (found live: the relay never connected,
	// on any deploy).
	// wss:// endpoints require TLS — a plaintext dial would leak the
	// handshake and fail (review finding 2).
	host := dialAddr(u)
	var conn net.Conn
	nd := &net.Dialer{Timeout: dialTimeout}
	if u.Scheme == "wss" {
		conn, err = (&tls.Dialer{NetDialer: nd}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = nd.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	br, err := handshake(conn, u)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	w := newWS(conn, br)
	go w.keepalive()
	return w, nil
}

// handshake sends the upgrade request and validates the 101 response. The
// returned reader must be used for frames: the server may send its first
// frame in the same packet as the response, and those bytes are already
// buffered (a single fixed-size Read lost them — review M4).
func handshake(conn net.Conn, u *url.URL) (*bufio.Reader, error) {
	key, err := wsKey()
	if err != nil {
		return nil, err
	}
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("ws handshake: %w", err)
	}
	// A 101 has no body; anything else is drained only for the message.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ws handshake: status %d %q", resp.StatusCode, string(body))
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(resp.Header, "Connection", "upgrade") {
		return nil, errors.New("ws handshake: missing upgrade headers")
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != acceptKey(key) {
		return nil, fmt.Errorf("ws handshake: bad Sec-WebSocket-Accept %q", got)
	}
	return br, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// wsKey is a fresh random Sec-WebSocket-Key (16 bytes, base64).
func wsKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// acceptKey is the Sec-WebSocket-Accept the server must answer key with.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// ws is a client websocket conn. Read returns whole data messages only:
// control frames are answered inside Read and fragments are reassembled.
type ws struct {
	c    net.Conn
	r    *bufio.Reader
	mu   sync.Mutex // serializes frame writes (acks, pongs, keepalive)
	done chan struct{}
	once sync.Once
}

func newWS(c net.Conn, r *bufio.Reader) *ws {
	return &ws{c: c, r: r, done: make(chan struct{})}
}

// Read blocks for the next complete data message. ctx cancellation closes
// the conn (unblocking the read); a frame silence longer than readTimeout
// is a dead socket.
func (w *ws) Read(ctx context.Context) ([]byte, error) {
	stop := context.AfterFunc(ctx, w.Close)
	defer stop()
	var (
		msg        []byte
		fragmented bool
	)
	for {
		_ = w.c.SetReadDeadline(time.Now().Add(readTimeout))
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch opcode {
		case opPing:
			if err := w.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue // the read deadline already moved; nothing else to do
		case opClose:
			_ = w.writeFrame(opClose, closeEcho(payload))
			return nil, fmt.Errorf("ws closed by peer%s", closeReason(payload))
		case opText, opBinary:
			if fragmented {
				return nil, errors.New("ws: new data frame inside a fragmented message")
			}
			if fin {
				return payload, nil
			}
			fragmented, msg = true, append(msg[:0], payload...)
		case opContinuation:
			if !fragmented {
				return nil, errors.New("ws: continuation frame without a message")
			}
			if len(msg)+len(payload) > maxMessage {
				return nil, fmt.Errorf("ws: message exceeds %d bytes", maxMessage)
			}
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("ws: unknown opcode %#x", opcode)
		}
	}
}

// readFrame reads one frame, enforcing the RFC's client-side rules: no
// RSV bits (no extensions negotiated), unmasked server frames, control
// frames unfragmented and ≤125 bytes, and a sane length before allocating.
func (w *ws) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(w.r, hdr[:]); err != nil {
		return
	}
	fin = hdr[0]&0x80 != 0
	opcode = hdr[0] & 0x0f
	if hdr[0]&0x70 != 0 {
		return fin, opcode, nil, errors.New("ws: reserved bits set")
	}
	if hdr[1]&0x80 != 0 {
		return fin, opcode, nil, errors.New("ws: server frame is masked")
	}
	var length uint64
	switch l := hdr[1] & 0x7f; l {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(w.r, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(w.r, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	default:
		length = uint64(l)
	}
	if opcode >= opClose && (length > 125 || !fin) {
		return fin, opcode, nil, errors.New("ws: invalid control frame")
	}
	if length > maxMessage {
		return fin, opcode, nil, fmt.Errorf("ws: frame length %d exceeds %d", length, maxMessage)
	}
	payload = make([]byte, length)
	_, err = io.ReadFull(w.r, payload)
	return
}

// Write sends one text message.
func (w *ws) Write(ctx context.Context, data []byte) error {
	return w.writeFrame(opText, data)
}

// writeFrame sends one masked, unfragmented frame with a fresh random mask
// (client frames must be masked — RFC 6455 §5.3). Pings, pongs and closes
// go through here with their own opcode: wrapping a pong in a text frame
// meant Slack never saw one (review M4).
func (w *ws) writeFrame(opcode byte, payload []byte) error {
	frame := make([]byte, 0, 14+len(payload))
	frame = append(frame, 0x80|opcode)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 1<<16:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := w.c.Write(frame)
	return err
}

// keepalive pings so a quiet-but-healthy socket keeps resetting the read
// deadline via pongs; a failed ping closes the conn, failing the Read.
func (w *ws) keepalive() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			if err := w.writeFrame(opPing, nil); err != nil {
				w.Close()
				return
			}
		}
	}
}

func (w *ws) Close() {
	w.once.Do(func() {
		close(w.done)
		_ = w.c.Close()
	})
}

// closeEcho is the close payload sent back: the peer's status code only.
func closeEcho(payload []byte) []byte {
	if len(payload) >= 2 {
		return payload[:2]
	}
	return nil
}

func closeReason(payload []byte) string {
	if len(payload) < 2 {
		return ""
	}
	return fmt.Sprintf(" (%d %s)", binary.BigEndian.Uint16(payload[:2]), string(payload[2:]))
}
