// Package regproxy exposes the cluster's built-in registry on a localhost port.
//
// Docker only speaks plain HTTP to registries on 127.0.0.0/8 without extra daemon config,
// so both the agent (pulls, with the node token) and `ogm push` (pushes, with an API key)
// run this proxy and address images as 127.0.0.1:PORT/{project}/{name}:{tag}.
package regproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prefix is the image-name prefix that refers to the cluster registry in specs.
const Prefix = "registry.ogm/"

// Proxy is a running localhost registry proxy.
type Proxy struct {
	Addr string // 127.0.0.1:PORT
	srv  *http.Server
	ln   net.Listener
}

// Start listens on 127.0.0.1:port (falling back to a random port if busy) and forwards
// /v2/ to server, authenticating with token. Extra headers are added to every request.
func Start(server, token string, port int, extra ...http.Header) (*Proxy, error) {
	u, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil && port != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			pr.Out.Header.Del("Authorization") // docker may send its own (empty) credentials
			pr.Out.Header.Set("Authorization", "Bearer "+token)
			for _, h := range extra {
				for k, v := range h {
					pr.Out.Header[k] = v
				}
			}
		},
		// Rewrite absolute Location headers from the server back to the proxy so uploads continue here.
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" {
				if lu, err := url.Parse(loc); err == nil && lu.IsAbs() {
					resp.Header.Set("Location", lu.RequestURI())
				}
			}
			return nil
		},
		FlushInterval: -1,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   16,
			ResponseHeaderTimeout: 2 * time.Minute,
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/v2/", rp)
	mux.Handle("/v2", rp)
	p := &Proxy{Addr: ln.Addr().String(), ln: ln, srv: &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}}
	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return p, nil
}

// Close stops the proxy.
func (p *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.srv.Shutdown(ctx)
}

// Rewrite maps "registry.ogm/x/y:t" to "127.0.0.1:PORT/x/y:t"; other images pass through.
func (p *Proxy) Rewrite(image string) string {
	if p == nil || !strings.HasPrefix(image, Prefix) {
		return image
	}
	return p.Addr + "/" + strings.TrimPrefix(image, Prefix)
}
