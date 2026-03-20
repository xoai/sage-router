package executor

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ClientPool maintains a set of reusable HTTP clients — one for direct
// connections and one per distinct proxy URL.
type ClientPool struct {
	mu      sync.RWMutex
	direct  *http.Client
	proxied map[string]*http.Client
}

// NewClientPool creates a ClientPool with a pre-configured direct client.
func NewClientPool() *ClientPool {
	return &ClientPool{
		direct:  newHTTPClient(nil),
		proxied: make(map[string]*http.Client),
	}
}

// Get returns an *http.Client for the given proxy URL. An empty proxyURL
// returns the direct (non-proxied) client. Clients are cached and reused.
func (p *ClientPool) Get(proxyURL string) *http.Client {
	if proxyURL == "" {
		return p.direct
	}

	// Fast path: check under read lock.
	p.mu.RLock()
	if c, ok := p.proxied[proxyURL]; ok {
		p.mu.RUnlock()
		return c
	}
	p.mu.RUnlock()

	// Slow path: create under write lock.
	p.mu.Lock()
	defer p.mu.Unlock()

	// Double-check after acquiring write lock.
	if c, ok := p.proxied[proxyURL]; ok {
		return c
	}

	parsed, err := url.Parse(proxyURL)
	if err != nil {
		// If the proxy URL is invalid, fall back to the direct client.
		return p.direct
	}

	c := newHTTPClient(parsed)
	p.proxied[proxyURL] = c
	return c
}

// newHTTPClient builds an *http.Client with sensible connection-pooling
// defaults. When proxy is non-nil, traffic is routed through that proxy.
func newHTTPClient(proxy *url.URL) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:         100,
		MaxIdleConnsPerHost:  10,
		IdleConnTimeout:      90 * time.Second,
		TLSHandshakeTimeout:  10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}

	return &http.Client{
		Transport: transport,
		// No global timeout — callers control deadlines via context.
	}
}
