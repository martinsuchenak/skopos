// Package srvurl lets every URL skopos works with name a DNS SRV record:
// srv+http://name/path and srv+https://name/path — knot's convention. name
// is looked up as an SRV record (Consul's skopos.service.consul, or
// _http._tcp.example.com); the records give the address and port.
//
// Inside skopos, Install registers an SRV-aware round tripper for both
// schemes on http.DefaultTransport, so every client built on it (the
// worker's, the CLI's, the log output's, the embeddings and Qdrant
// clients) takes srv+ URLs as they are: each new connection looks the
// record up again (a rescheduled service with a new port is followed), and
// tries the targets in RFC 2782 order. The request keeps the name as its
// host, so TLS verifies against it.
//
// URLs handed to other programs (an agent's MCP config, git push, `skopos
// install`) can't carry srv+: Resolve turns one into a plain URL at that
// moment.
package srvurl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const prefix = "srv+"

// IsSRV reports whether raw is a srv+http(s) URL.
func IsSRV(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	return strings.HasPrefix(s, "srv+http://") || strings.HasPrefix(s, "srv+https://")
}

// Plain strips srv+ from a scheme ("srv+https" → "https"); other schemes
// are returned as they are.
func Plain(scheme string) string {
	if strings.HasPrefix(strings.ToLower(scheme), prefix) {
		return scheme[len(prefix):]
	}
	return scheme
}

// HTTPScheme: http or https, with or without srv+.
func HTTPScheme(scheme string) bool {
	s := strings.ToLower(Plain(scheme))
	return s == "http" || s == "https"
}

var (
	mu       sync.RWMutex
	resolver = net.DefaultResolver
)

// SetDNSServer sends SRV (and their targets') lookups to one DNS server,
// "host:port" (Consul: 127.0.0.1:8600); "" = the system resolver.
func SetDNSServer(addr string) error {
	addr = strings.TrimSpace(addr)
	mu.Lock()
	defer mu.Unlock()
	if addr == "" {
		resolver = net.DefaultResolver
		return nil
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("dns server %q: want host:port", addr)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}}
	return nil
}

func currentResolver() *net.Resolver {
	mu.RLock()
	defer mu.RUnlock()
	return resolver
}

// lookupSRV is swapped in tests.
var lookupSRV = func(ctx context.Context, name string) ([]*net.SRV, error) {
	_, recs, err := currentResolver().LookupSRV(ctx, "", "", name)
	return recs, err
}

// SetLookup replaces the SRV lookup (tests in other packages point srv+
// names at local servers); it returns the restore function.
func SetLookup(fn func(ctx context.Context, name string) ([]*net.SRV, error)) (restore func()) {
	old := lookupSRV
	lookupSRV = fn
	return func() { lookupSRV = old }
}

// targets returns the record's host:port addresses in RFC 2782 order (Go
// sorts by priority and shuffles by weight).
func targets(ctx context.Context, name string) ([]string, error) {
	recs, err := lookupSRV(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("srv lookup %s: %w", name, err)
	}
	var out []string
	for _, r := range recs {
		if r.Port == 0 || r.Target == "" || r.Target == "." {
			continue
		}
		out = append(out, net.JoinHostPort(strings.TrimSuffix(r.Target, "."), strconv.Itoa(int(r.Port))))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("srv lookup %s: no usable records", name)
	}
	return out, nil
}

// dial connects to the first target that answers; addr's host is the SRV
// name (its port, the scheme default, is ignored).
func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	name, _, err := net.SplitHostPort(addr)
	if err != nil {
		name = addr
	}
	addrs, err := targets(ctx, name)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Resolver: currentResolver()}
	var errs []error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, network, a)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, errors.Join(errs...)
}

// roundTripper serves srv+ requests: the scheme loses srv+, the host stays
// the name (TLS verifies it), and connections dial the SRV targets.
type roundTripper struct{ inner *http.Transport }

func (t *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = Plain(r.URL.Scheme)
	return t.inner.RoundTrip(r)
}

// Transport returns a round tripper for srv+ requests built on base (its
// TLS and proxy settings; its dialer is replaced).
func Transport(base *http.Transport) http.RoundTripper {
	inner := base.Clone()
	inner.DialContext = dial
	inner.DialTLSContext = nil
	inner.Proxy = nil // the targets come from DNS; a proxy would see the bare name
	return &roundTripper{inner: inner}
}

var installOnce sync.Once

// Install registers the srv+http and srv+https schemes on
// http.DefaultTransport (once; safe to call again).
func Install() {
	installOnce.Do(func() {
		dt, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return
		}
		rt := Transport(dt)
		dt.RegisterProtocol("srv+http", rt)
		dt.RegisterProtocol("srv+https", rt)
	})
}

// Resolve turns a srv+ URL into a plain one for programs that can't look
// SRV up themselves, from the record's first target: https keeps the name
// (TLS needs it) with the record's port; http uses the target's address,
// so it works even where only the configured DNS server knows the name.
// Other URLs are returned unchanged.
func Resolve(ctx context.Context, raw string) (string, error) {
	if !IsSRV(raw) {
		return raw, nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parsing %q: %w", Redacted(raw), err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := targets(ctx, u.Hostname())
	if err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(addrs[0])
	u.Scheme = Plain(u.Scheme)
	if strings.EqualFold(u.Scheme, "https") {
		u.Host = net.JoinHostPort(u.Hostname(), port)
		return u.String(), nil
	}
	if ips, err := currentResolver().LookupHost(ctx, host); err == nil && len(ips) > 0 {
		host = ips[0]
	}
	u.Host = net.JoinHostPort(host, port)
	return u.String(), nil
}

// Redacted hides a URL's password (for messages).
func Redacted(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparsable url)"
	}
	return u.Redacted()
}
