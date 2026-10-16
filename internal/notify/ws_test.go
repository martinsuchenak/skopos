package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pipeWS is a client ws over net.Pipe; the returned conn is the server end.
func pipeWS(t *testing.T) (*ws, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	w := newWS(client, bufio.NewReader(client))
	t.Cleanup(func() { w.Close(); server.Close() })
	return w, server
}

// serverFrame encodes an unmasked server→client frame.
func serverFrame(fin bool, opcode byte, payload []byte) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	switch n := len(payload); {
	case n < 126:
		out = append(out, byte(n))
	case n < 1<<16:
		out = append(out, 126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	default:
		out = append(out, 127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	return append(out, payload...)
}

// readClientFrame decodes one client→server frame, asserting it is masked.
func readClientFrame(t *testing.T, r io.Reader) (byte, []byte) {
	t.Helper()
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		t.Errorf("read client frame: %v", err)
		return 0, nil
	}
	if hdr[0]&0x80 == 0 {
		t.Errorf("client frame must be final, got %#x", hdr[0])
	}
	if hdr[1]&0x80 == 0 {
		t.Errorf("client frame must be masked")
	}
	length := uint64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		_, _ = io.ReadFull(r, ext[:])
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		_, _ = io.ReadFull(r, ext[:])
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	_, _ = io.ReadFull(r, mask[:])
	payload := make([]byte, length)
	_, _ = io.ReadFull(r, payload)
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return hdr[0] & 0x0f, payload
}

// A ping is answered with a real pong frame (opcode 0xA, same payload) —
// it used to be wrapped in a masked TEXT frame, so Slack never saw a pong.
func TestWSPingGetsPong(t *testing.T) {
	w, server := pipeWS(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = server.Write(serverFrame(true, opPing, []byte("abcd")))
		op, payload := readClientFrame(t, server)
		if op != opPong || string(payload) != "abcd" {
			t.Errorf("pong: opcode %#x payload %q", op, payload)
		}
		_, _ = server.Write(serverFrame(true, opText, []byte(`{"type":"hello"}`)))
	}()
	got, err := w.Read(context.Background())
	if err != nil || string(got) != `{"type":"hello"}` {
		t.Fatalf("Read after ping: %q err %v", got, err)
	}
	<-done
}

// Extended lengths decode both ways: 16-bit (126) and 64-bit (127).
func TestWSExtendedLengths(t *testing.T) {
	for _, n := range []int{125, 126, 300, 65535, 65536, 70000} {
		w, server := pipeWS(t)
		want := bytes.Repeat([]byte{'x'}, n)
		go func() { _, _ = server.Write(serverFrame(true, opText, want)) }()
		got, err := w.Read(context.Background())
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("read len %d: got %d err %v", n, len(got), err)
		}
		errc := make(chan error, 1)
		go func() { errc <- w.Write(context.Background(), want) }()
		op, payload := readClientFrame(t, server)
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		if op != opText || !bytes.Equal(payload, want) {
			t.Fatalf("write len %d: opcode %#x len %d", n, op, len(payload))
		}
	}
}

// An absurd extended length is rejected before anything is allocated.
func TestWSRejectsOversizeFrame(t *testing.T) {
	for _, n := range []uint64{maxMessage + 1, 1 << 40, 1 << 63} {
		w, server := pipeWS(t)
		hdr := binary.BigEndian.AppendUint64([]byte{0x81, 127}, n)
		go func() { _, _ = server.Write(hdr) }()
		if _, err := w.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("length %d: want oversize error, got %v", n, err)
		}
	}
}

// Fragments reassemble, with a control frame allowed in between; protocol
// violations are errors rather than garbage messages.
func TestWSFragmentsAndViolations(t *testing.T) {
	w, server := pipeWS(t)
	go func() {
		_, _ = server.Write(serverFrame(false, opText, []byte("hel")))
		_, _ = server.Write(serverFrame(true, opPing, nil))
		readClientFrame(t, server) // the pong
		_, _ = server.Write(serverFrame(false, opContinuation, []byte("lo ")))
		_, _ = server.Write(serverFrame(true, opContinuation, []byte("world")))
	}()
	got, err := w.Read(context.Background())
	if err != nil || string(got) != "hello world" {
		t.Fatalf("reassembled: %q err %v", got, err)
	}

	for name, frame := range map[string][]byte{
		"orphan continuation": serverFrame(true, opContinuation, []byte("x")),
		"masked server frame": {0x81, 0x80 | 1, 0, 0, 0, 0, 'x'},
		"reserved bits":       {0xc1, 1, 'x'},
		"fragmented control":  serverFrame(false, opPing, nil),
		"oversize control":    serverFrame(true, opPing, bytes.Repeat([]byte{'p'}, 126)),
	} {
		w, server := pipeWS(t)
		go func() { _, _ = server.Write(frame) }()
		if _, err := w.Read(context.Background()); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	w, server = pipeWS(t)
	go func() {
		_, _ = server.Write(serverFrame(false, opText, []byte("a")))
		_, _ = server.Write(serverFrame(true, opText, []byte("b")))
	}()
	if _, err := w.Read(context.Background()); err == nil {
		t.Error("new data frame mid-message: want error")
	}
}

// A close frame ends Read with an error and is echoed back.
func TestWSCloseFrame(t *testing.T) {
	w, server := pipeWS(t)
	go func() {
		_, _ = server.Write(serverFrame(true, opClose, append([]byte{0x03, 0xe9}, "bye"...)))
		op, payload := readClientFrame(t, server)
		if op != opClose || !bytes.Equal(payload, []byte{0x03, 0xe9}) {
			t.Errorf("close echo: %#x %v", op, payload)
		}
	}()
	if _, err := w.Read(context.Background()); err == nil || !strings.Contains(err.Error(), "1001") {
		t.Fatalf("close: %v", err)
	}
}

// Cancelling ctx unblocks a Read on a silent socket (it used to ignore ctx
// and block forever on a half-open connection).
func TestWSReadHonorsContext(t *testing.T) {
	w, _ := pipeWS(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := w.Read(ctx)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("Read must return on cancel: err %v after %s", err, time.Since(start))
	}
}

// fakeServer accepts one upgrade, answers with respond(key), then writes
// trailing bytes in the SAME write — a client that reads the handshake
// with one fixed-size Read loses them.
func fakeServer(t *testing.T, respond func(key string) string, trailing []byte) (string, <-chan *http.Request) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqs := make(chan *http.Request, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		reqs <- req
		_, _ = conn.Write(append([]byte(respond(req.Header.Get("Sec-WebSocket-Key"))), trailing...))
		_, _ = io.Copy(io.Discard, conn) // hold open until the client closes
	}()
	return "ws://" + ln.Addr().String() + "/link/?ticket=abc", reqs
}

func upgradeResponse(accept string) string {
	return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
}

func TestWSHandshake(t *testing.T) {
	hello := serverFrame(true, opText, []byte(`{"type":"hello"}`))
	url, reqs := fakeServer(t, func(key string) string { return upgradeResponse(acceptKey(key)) }, hello)
	conn, err := dialWS(context.Background(), url)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	req := <-reqs
	if req.URL.RequestURI() != "/link/?ticket=abc" || req.Header.Get("Sec-WebSocket-Version") != "13" ||
		!strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("upgrade request: %s %v", req.URL, req.Header)
	}
	if k, err := base64.StdEncoding.DecodeString(req.Header.Get("Sec-WebSocket-Key")); err != nil || len(k) != 16 {
		t.Fatalf("key must be 16 random bytes: %v", err)
	}
	// The frame that arrived with the 101 must not be lost.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := conn.Read(ctx)
	if err != nil || string(got) != `{"type":"hello"}` {
		t.Fatalf("first frame after handshake: %q err %v", got, err)
	}
}

func TestWSHandshakeRejects(t *testing.T) {
	for name, respond := range map[string]func(string) string{
		"bad accept": func(string) string { return upgradeResponse("bm90LXRoZS1yaWdodC1hY2NlcHQ=") },
		"no accept":  func(string) string { return "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n" },
		"not 101": func(string) string {
			return "HTTP/1.1 400 Bad Request\r\nContent-Length: 11\r\n\r\nbad request"
		},
		"no upgrade": func(key string) string {
			return "HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"
		},
	} {
		url, _ := fakeServer(t, respond, nil)
		if conn, err := dialWS(context.Background(), url); err == nil {
			conn.Close()
			t.Errorf("%s: handshake must fail", name)
		}
	}
}

// RFC 6455 §1.3 sample: the accept derivation is the spec's.
func TestAcceptKeyRFCSample(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("acceptKey = %q", got)
	}
	a, _ := wsKey()
	b, _ := wsKey()
	if a == b {
		t.Fatal("handshake keys must be random")
	}
}
