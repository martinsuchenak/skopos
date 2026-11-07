package srvurl

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeSRV answers lookups for name with the given host:port targets.
func fakeSRV(t *testing.T, name string, addrs ...string) {
	t.Helper()
	old := lookupSRV
	t.Cleanup(func() { lookupSRV = old })
	lookupSRV = func(_ context.Context, n string) ([]*net.SRV, error) {
		if n != name {
			return nil, errors.New("no such host")
		}
		var out []*net.SRV
		for i, a := range addrs {
			h, p, _ := net.SplitHostPort(a)
			port, _ := strconv.Atoi(p)
			out = append(out, &net.SRV{Target: h + ".", Port: uint16(port), Priority: uint16(i)})
		}
		return out, nil
	}
}

func deadAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}

// A srv+http URL works with the default client: the record's targets are
// dialled in order (the dead first one is skipped), the request keeps the
// name as its Host, and the path and query survive.
func TestDefaultClientTakesSRVURLs(t *testing.T) {
	Install()
	Install() // safe twice
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.RequestURI()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	fakeSRV(t, "skopos.service.consul", deadAddr(t), strings.TrimPrefix(srv.URL, "http://"))

	resp, err := http.Get("srv+http://skopos.service.consul/api/whoami?x=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" || gotHost != "skopos.service.consul" || gotPath != "/api/whoami?x=1" {
		t.Fatalf("body %q host %q path %q", body, gotHost, gotPath)
	}
	if _, err := http.Get("srv+http://unknown.service.consul/"); err == nil || !strings.Contains(err.Error(), "srv lookup unknown.service.consul") {
		t.Fatalf("unknown name: %v", err)
	}
}

// Resolve: http gets the target's address and port; https keeps the name
// (TLS) with the record's port; plain URLs pass through.
func TestResolve(t *testing.T) {
	fakeSRV(t, "skopos.service.consul", "127.0.0.1:24567")
	got, err := Resolve(context.Background(), "srv+http://skopos.service.consul/mcp")
	if err != nil || got != "http://127.0.0.1:24567/mcp" {
		t.Fatalf("http: %q %v", got, err)
	}
	got, err = Resolve(context.Background(), "SRV+HTTPS://user:pw@skopos.service.consul/x")
	if err != nil || got != "https://user:pw@skopos.service.consul:24567/x" {
		t.Fatalf("https: %q %v", got, err)
	}
	if got, _ := Resolve(context.Background(), "https://skopos.example.com"); got != "https://skopos.example.com" {
		t.Fatalf("plain: %q", got)
	}
	if _, err := Resolve(context.Background(), "srv+http://nothing.consul/"); err == nil {
		t.Fatal("unknown name must fail")
	}
}

func TestSchemesAndDNSServer(t *testing.T) {
	for raw, want := range map[string]bool{"srv+http://a/b": true, " SRV+HTTPS://a": true, "http://a": false, "srv+ftp://a": false} {
		if IsSRV(raw) != want {
			t.Errorf("IsSRV(%q) != %v", raw, want)
		}
	}
	if !HTTPScheme("srv+https") || !HTTPScheme("http") || HTTPScheme("ftp") || Plain("srv+http") != "http" {
		t.Fatal("schemes")
	}
	if err := SetDNSServer("127.0.0.1"); err == nil {
		t.Fatal("a server without a port must be refused")
	}
	if err := SetDNSServer("127.0.0.1:8600"); err != nil || currentResolver() == net.DefaultResolver {
		t.Fatalf("set: %v", err)
	}
	_ = SetDNSServer("")
	if currentResolver() != net.DefaultResolver {
		t.Fatal("reset")
	}
}
