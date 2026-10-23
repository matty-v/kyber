package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Keep client-go's TLS, proxy, authentication and SPDY stream implementations.
// Its upgrade RoundTrip waits for headers without closing the socket on context
// cancellation, so own that small handshake step and cancel the socket too.
func newExecExecutor(ctx context.Context, config *rest.Config, target *url.URL) (remotecommand.Executor, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}
	proxy := config.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	dialer, err := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{TLS: tlsConfig, Proxier: proxy, PingPeriod: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	upgrade := &execUpgradeTransport{ctx: ctx, dialer: dialer}
	wrapped, err := rest.HTTPWrappersForConfig(config, upgrade)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewSPDYExecutorForTransports(wrapped, upgrade, http.MethodPost, target)
}

type execUpgradeTransport struct {
	ctx    context.Context
	dialer *spdy.SpdyRoundTripper
	conn   *execTransportConn
}

func (t *execUpgradeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	req.Header.Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Add(httpstream.HeaderUpgrade, spdy.HeaderSpdy31)
	conn, err := t.dialer.Dial(req)
	if err != nil {
		return nil, err
	}
	wrapped := &execTransportConn{Conn: conn, closed: make(chan struct{})}
	t.conn = wrapped
	go func() {
		select {
		case <-t.ctx.Done():
			_ = wrapped.Close()
		case <-wrapped.closed:
		}
	}()
	response, err := http.ReadResponse(bufio.NewReader(wrapped), req)
	if err != nil {
		_ = wrapped.Close()
		return nil, err
	}
	// Exec never needs redirects; do not forward credential-bearing requests to
	// another endpoint or leave a previous upgrade connection outstanding.
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		_ = wrapped.Close()
		_ = response.Body.Close()
		return nil, fmt.Errorf("Kubernetes exec upgrade returned a redirect")
	}
	return response, nil
}

func (t *execUpgradeTransport) NewConnection(response *http.Response) (httpstream.Connection, error) {
	if t.conn == nil {
		return nil, fmt.Errorf("Kubernetes exec upgrade has no connection")
	}
	if response.StatusCode != http.StatusSwitchingProtocols ||
		!strings.Contains(strings.ToLower(response.Header.Get(httpstream.HeaderConnection)), "upgrade") ||
		!strings.Contains(strings.ToLower(response.Header.Get(httpstream.HeaderUpgrade)), strings.ToLower(spdy.HeaderSpdy31)) {
		_ = t.conn.Close()
		_ = response.Body.Close()
		return nil, fmt.Errorf("Kubernetes exec upgrade failed (HTTP %d)", response.StatusCode)
	}
	return spdy.NewClientConnectionWithPings(t.conn, 5*time.Second)
}

type execTransportConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *execTransportConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
