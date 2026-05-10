package valkey

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/fortix/go-libs/netx/dns"
	"github.com/paularlott/logger"
	valkeygo "github.com/valkey-io/valkey-go"
)

type Client struct {
	vk  valkeygo.Client
	log logger.Logger
}

// ResolveHost resolves a host string to an address.
// If the host starts with "_", it's treated as an SRV name and resolved via DNS.
// Otherwise, it's parsed as host:port (e.g., "localhost:6379").
func ResolveHost(log logger.Logger, host string) (string, int, error) {
	// SRV names start with underscore (e.g., _valkey._tcp.myserver)
	if strings.HasPrefix(host, "_") {
		resolver := dns.NewDNSResolver(dns.ResolverConfig{Logger: log})
		addrs, err := resolver.LookupSRV(host)
		if err != nil {
			return "", 0, fmt.Errorf("SRV lookup failed for %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return "", 0, fmt.Errorf("no addresses found for SRV %s", host)
		}
		return addrs[0].IP.String(), addrs[0].Port, nil
	}

	// Parse as host:port
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return "", 0, fmt.Errorf("invalid host:port format %s: %w", host, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %s: %w", p, err)
	}
	return h, port, nil
}

func NewClient(log logger.Logger, host string, password string) (*Client, error) {
	resolvedHost, port, err := ResolveHost(log, host)
	if err != nil {
		return nil, fmt.Errorf("resolving valkey host: %w", err)
	}

	vk, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress: []string{fmt.Sprintf("%s:%d", resolvedHost, port)},
		Password:    password,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to valkey: %w", err)
	}

	log.Info("connected to valkey", "host", resolvedHost, "port", port)
	return &Client{vk: vk, log: log}, nil
}

func (c *Client) Close() {
	c.vk.Close()
}
