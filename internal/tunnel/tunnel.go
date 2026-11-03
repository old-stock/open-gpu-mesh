// Package tunnel provides the multiplexed WebSocket tunnel between agents and the server.
package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 30 * time.Second
	cfg.MaxStreamWindowSize = 4 << 20
	cfg.StreamOpenTimeout = 30 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}

// Accept upgrades an HTTP request into a yamux server session.
func Accept(w http.ResponseWriter, r *http.Request) (*yamux.Session, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return yamux.Server(conn, yamuxConfig())
}

// Dial connects to the server and returns a yamux client session.
func Dial(ctx context.Context, serverURL, token string) (*yamux.Session, error) {
	u := strings.TrimRight(serverURL, "/") + "/v1/agent/connect"
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("connect %s: %s: %s", u, resp.Status, strings.TrimSpace(string(b)))
		}
		return nil, err
	}
	c.SetReadLimit(-1)
	conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
	return yamux.Client(conn, yamuxConfig())
}

// DialError marks a failure that happened before any request bytes reached the backend.
type DialError struct{ Err error }

func (e *DialError) Error() string { return "tunnel dial: " + e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// OpenProxy opens a data stream to an instance port through a session.
func OpenProxy(ctx context.Context, sess *yamux.Session, instanceID string, port int) (net.Conn, error) {
	st, err := sess.OpenStream()
	if err != nil {
		return nil, &DialError{err}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(dl)
	} else {
		_ = st.SetDeadline(time.Now().Add(30 * time.Second))
	}
	hdr, _ := json.Marshal(proto.StreamHeader{Kind: "proxy", InstanceID: instanceID, Port: port})
	if _, err := st.Write(append(hdr, '\n')); err != nil {
		st.Close()
		return nil, &DialError{err}
	}
	ack := make([]byte, 1)
	if _, err := io.ReadFull(st, ack); err != nil {
		st.Close()
		return nil, &DialError{err}
	}
	if ack[0] != proto.AckOK {
		st.Close()
		return nil, &DialError{fmt.Errorf("instance %s port %d refused connection", instanceID, port)}
	}
	_ = st.SetDeadline(time.Time{})
	return st, nil
}

// ReadHeader reads a StreamHeader line from a freshly accepted stream.
func ReadHeader(c net.Conn) (proto.StreamHeader, error) {
	var h proto.StreamHeader
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for {
		if _, err := c.Read(one); err != nil {
			return h, err
		}
		if one[0] == '\n' {
			break
		}
		buf = append(buf, one[0])
		if len(buf) > 4096 {
			return h, fmt.Errorf("stream header too long")
		}
	}
	err := json.Unmarshal(buf, &h)
	return h, err
}

// Splice copies bytes both ways until either side closes.
func Splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
