package assess

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// speedTarget describes a speed-test server.
type speedTarget struct {
	ping string                   // cheap GET used for RTT
	down func(bytes int64) string // GET returning that many bytes
	up   string                   // POST endpoint that discards the body
	auth string                   // optional Bearer token
}

func hubTarget(server, token string) speedTarget {
	s := strings.TrimRight(server, "/")
	return speedTarget{
		ping: s + "/v1/speedtest/ping",
		down: func(n int64) string { return fmt.Sprintf("%s/v1/speedtest/down?bytes=%d", s, n) },
		up:   s + "/v1/speedtest/up",
		auth: token,
	}
}

func cloudflareTarget() speedTarget {
	return speedTarget{
		ping: "https://speed.cloudflare.com/__down?bytes=0",
		down: func(n int64) string { return fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", n) },
		up:   "https://speed.cloudflare.com/__up",
	}
}

func (t speedTarget) req(ctx context.Context, method, url string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if t.auth != "" {
		r.Header.Set("Authorization", "Bearer "+t.auth)
	}
	r.Header.Set("User-Agent", "ogm-assess")
	return r, nil
}

// measureRTT returns the median round-trip time (ms) of small requests over a warm
// connection, so TCP/TLS setup is excluded.
func measureRTT(ctx context.Context, c *http.Client, t speedTarget) (float64, error) {
	var samples []float64
	for i := 0; i < 6; i++ {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		r, err := t.req(ctx, http.MethodGet, t.ping, nil)
		if err != nil {
			cancel()
			return 0, err
		}
		start := time.Now()
		resp, err := c.Do(r)
		if err != nil {
			cancel()
			return 0, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode >= 300 {
			return 0, fmt.Errorf("%s: HTTP %d", t.ping, resp.StatusCode)
		}
		if i > 0 { // first request pays for connection setup
			samples = append(samples, float64(time.Since(start).Microseconds())/1000)
		}
	}
	sort.Float64s(samples)
	return round1(samples[len(samples)/2]), nil
}

const (
	speedStreams = 4
	speedChunk   = 25 << 20 // bytes per request; streams loop until time is up
	speedWarmup  = 1 * time.Second
)

// rate runs fn on several parallel streams for d and returns Mbit/s, ignoring the first
// second (TCP slow start). fn must add transferred bytes to n and return when ctx is done.
func rate(ctx context.Context, d time.Duration, fn func(ctx context.Context, n *atomic.Int64) error) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	var n atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, speedStreams)
	for i := 0; i < speedStreams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := fn(ctx, &n); err != nil && ctx.Err() == nil {
					errs <- err
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); cancel() }() // all streams failed early: stop waiting
	var base int64
	var baseT time.Time
	select {
	case <-time.After(speedWarmup):
		base, baseT = n.Load(), time.Now()
	case <-ctx.Done():
	}
	<-ctx.Done()
	end, endT := n.Load(), time.Now() // snapshot before waiting for in-flight responses
	wg.Wait()
	close(errs)
	if baseT.IsZero() {
		return 0, ctx.Err()
	}
	bytes, secs := end-base, endT.Sub(baseT).Seconds()
	if bytes <= 0 {
		if err := <-errs; err != nil {
			return 0, err
		}
		return 0, errors.New("no data transferred")
	}
	return round1(float64(bytes) * 8 / secs / 1e6), nil
}

type countingWriter struct{ n *atomic.Int64 }

func (w countingWriter) Write(p []byte) (int, error) { w.n.Add(int64(len(p))); return len(p), nil }

func measureDown(ctx context.Context, c *http.Client, t speedTarget, d time.Duration) (float64, error) {
	return rate(ctx, d, func(ctx context.Context, n *atomic.Int64) error {
		r, err := t.req(ctx, http.MethodGet, t.down(speedChunk), nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(r)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("download: HTTP %d", resp.StatusCode)
		}
		_, err = io.Copy(countingWriter{n}, resp.Body)
		return err
	})
}

// uploadBody yields random data until size bytes or the deadline, then EOF (so the request
// completes normally rather than being cancelled mid-body).
type uploadBody struct {
	ctx  context.Context
	left int64
	buf  []byte
	n    *atomic.Int64
}

func (b *uploadBody) Read(p []byte) (int, error) {
	if b.left <= 0 || b.ctx.Err() != nil {
		return 0, io.EOF
	}
	k := copy(p, b.buf)
	if int64(k) > b.left {
		k = int(b.left)
	}
	b.left -= int64(k)
	b.n.Add(int64(k))
	return k, nil
}

func measureUp(ctx context.Context, c *http.Client, t speedTarget, d time.Duration) (float64, error) {
	buf := make([]byte, 256<<10)
	_, _ = crand.Read(buf)
	// The body must outlive the measurement context only until EOF; use a deadline-aware
	// context for the body but not for the request, so the final response is still read.
	return rate(ctx, d, func(mctx context.Context, n *atomic.Int64) error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(mctx), d+30*time.Second)
		defer cancel()
		r, err := t.req(rctx, http.MethodPost, t.up, &uploadBody{ctx: mctx, left: speedChunk, buf: buf, n: n})
		if err != nil {
			return err
		}
		r.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.Do(r)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("upload: HTTP %d", resp.StatusCode)
		}
		return nil
	})
}

// ---------- NAT type via STUN (RFC 5389 binding requests) ----------

var stunServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}

// natType sends binding requests to two STUN servers from one UDP socket and compares the
// public mappings: identical mapping = endpoint-independent NAT (peer-to-peer works);
// different = symmetric NAT (needs a relay); no answer = UDP blocked.
func natType(ctx context.Context) string {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return ""
	}
	defer conn.Close()
	var mapped []netip.AddrPort
	for _, s := range stunServers {
		if ap, err := stunBinding(ctx, conn, s); err == nil {
			mapped = append(mapped, ap)
		}
	}
	switch {
	case len(mapped) == 0:
		return "udp-blocked"
	case isLocalAddr(mapped[0].Addr()):
		return "none"
	case len(mapped) == 1:
		return "unknown"
	case mapped[0] == mapped[1]:
		return "endpoint-independent"
	default:
		return "symmetric"
	}
}

const stunMagic = 0x2112A442

func stunBinding(ctx context.Context, conn *net.UDPConn, server string) (netip.AddrPort, error) {
	raddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return netip.AddrPort{}, err
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:], 0x0001) // binding request, no attributes
	binary.BigEndian.PutUint32(req[4:], stunMagic)
	_, _ = crand.Read(req[8:20])
	buf := make([]byte, 1500)
	for attempt := 0; attempt < 3 && ctx.Err() == nil; attempt++ {
		if _, err := conn.WriteToUDP(req, raddr); err != nil {
			return netip.AddrPort{}, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				break // timeout: retransmit
			}
			if ap, ok := parseStunResponse(buf[:n], req[8:20]); ok {
				return ap, nil
			}
		}
	}
	return netip.AddrPort{}, errors.New("no STUN response from " + server)
}

// parseStunResponse extracts the (XOR-)MAPPED-ADDRESS from a binding success response.
func parseStunResponse(b, txid []byte) (netip.AddrPort, bool) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != 0x0101 || binary.BigEndian.Uint32(b[4:]) != stunMagic || string(b[8:20]) != string(txid) {
		return netip.AddrPort{}, false
	}
	var plain netip.AddrPort
	for p := 20; p+4 <= len(b); {
		typ, l := binary.BigEndian.Uint16(b[p:]), int(binary.BigEndian.Uint16(b[p+2:]))
		v := b[p+4:]
		if len(v) < l {
			break
		}
		v = v[:l]
		if l >= 8 && v[1] == 0x01 { // IPv4
			port := binary.BigEndian.Uint16(v[2:])
			var ip [4]byte
			copy(ip[:], v[4:8])
			switch typ {
			case 0x0020: // XOR-MAPPED-ADDRESS
				port ^= stunMagic >> 16
				binary.BigEndian.PutUint32(ip[:], binary.BigEndian.Uint32(ip[:])^stunMagic)
				return netip.AddrPortFrom(netip.AddrFrom4(ip), port), true
			case 0x0001: // MAPPED-ADDRESS
				plain = netip.AddrPortFrom(netip.AddrFrom4(ip), port)
			}
		}
		p += 4 + (l+3)&^3
	}
	return plain, plain.IsValid()
}

func isLocalAddr(a netip.Addr) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, ia := range addrs {
		if n, ok := ia.(*net.IPNet); ok {
			if la, ok := netip.AddrFromSlice(n.IP); ok && la.Unmap() == a {
				return true
			}
		}
	}
	return false
}

// hasIPv6 reports whether the machine has a global IPv6 route (no packets are sent).
func hasIPv6() bool {
	c, err := net.Dial("udp6", "[2606:4700:4700::1111]:53")
	if err != nil {
		return false
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	return ok && ua.IP.IsGlobalUnicast() && !ua.IP.IsPrivate()
}
