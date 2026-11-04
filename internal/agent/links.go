package agent

import (
	"context"
	"hash/fnv"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
	"github.com/old-stock/open-gpu-mesh/internal/tunnel"
)

// LinkHoster is implemented by runtimes that can tell where link listeners should bind
// (an address containers can reach) and which host name containers use for it.
type LinkHoster interface {
	LinkHost(ctx context.Context) (listenIP, containerHost string)
}

// AddrLister is implemented by containers with their own network address, so a link
// listener on a shared bridge can tell its own container from its neighbours.
type AddrLister interface {
	IPs(ctx context.Context) []string
}

// VolumeRemover is implemented by runtimes that support persistent volumes.
type VolumeRemover interface {
	RemoveVolumes(ctx context.Context, names []string) error
}

// linkSet holds the listeners that forward an instance's {{link:GROUP:PORT}} connections
// to the hub, which relays them to a ready instance of GROUP.
type linkSet struct {
	host  string                // host name the container dials
	addrs map[proto.Link]string // link -> host:port as seen from the container
	lns   []net.Listener
}

func (l *linkSet) close() {
	if l == nil {
		return
	}
	for _, ln := range l.lns {
		ln.Close()
	}
}

// linkPort picks a stable port per (instance, link) so a re-attached container (agent
// restart) finds its links on the same address.
func linkPort(instanceID string, l proto.Link, attempt int) int {
	h := fnv.New32a()
	h.Write([]byte(instanceID + "/" + l.Group + "/" + strconv.Itoa(l.Port)))
	return 20000 + int((h.Sum32()+uint32(attempt)*7919)%20000)
}

func (i *instance) openLinks(ctx context.Context, spec proto.InstanceSpec) (*linkSet, error) {
	if len(spec.Links) == 0 {
		return nil, nil
	}
	listenIP, host := "127.0.0.1", i.a.rt.IMDSHost()
	if lh, ok := i.a.rt.(LinkHoster); ok {
		listenIP, host = lh.LinkHost(ctx)
	}
	ls := &linkSet{host: host, addrs: map[proto.Link]string{}}
	for _, l := range spec.Links {
		var ln net.Listener
		var err error
		for attempt := 0; attempt < 20; attempt++ {
			ln, err = net.Listen("tcp", net.JoinHostPort(listenIP, strconv.Itoa(linkPort(spec.ID, l, attempt))))
			if err == nil {
				break
			}
		}
		if err != nil {
			ls.close()
			return nil, err
		}
		ls.lns = append(ls.lns, ln)
		port := ln.Addr().(*net.TCPAddr).Port
		ls.addrs[l] = net.JoinHostPort(host, strconv.Itoa(port))
		go i.serveLink(ln, spec.ID, l)
	}
	return ls, nil
}

func (i *instance) serveLink(ln net.Listener, instanceID string, l proto.Link) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			if !i.linkPeerAllowed(c) {
				i.a.log.Warn("link connection refused: not from the owning container", "instance", instanceID, "from", c.RemoteAddr().String())
				c.Close()
				return
			}
			i.a.mu.Lock()
			sess := i.a.sess
			i.a.mu.Unlock()
			if sess == nil {
				c.Close()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			st, err := tunnel.OpenLink(ctx, sess, instanceID, l.Group, l.Port)
			cancel()
			if err != nil {
				i.a.log.Debug("link failed", "instance", instanceID, "group", l.Group, "port", l.Port, "err", err)
				c.Close()
				return
			}
			tunnel.Splice(c, st)
		}()
	}
}

// expandLinks replaces {{link:G:P}}, {{link_host:G:P}} and {{link_port:G:P}} in v.
func (l *linkSet) expand(v string) string {
	if l == nil {
		return v
	}
	for _, r := range api.ParseRefs(v) {
		if r.Kind == "secret" {
			continue
		}
		addr, ok := l.addrs[proto.Link{Group: r.Name, Port: r.Port}]
		if !ok {
			continue
		}
		host, port, _ := net.SplitHostPort(addr)
		rep := addr
		switch r.Kind {
		case "link_host":
			rep = host
		case "link_port":
			rep = port
		}
		v = strings.ReplaceAll(v, r.Raw, rep)
	}
	return v
}

// linkPeerAllowed accepts a link connection only from the instance's own container.
// Listeners sit on the shared Docker bridge, so without this check any container on
// the node (another tenant's included) could use this instance's links. Loopback
// peers (process runtime, Docker Desktop's VM forwarding) can only be host processes.
func (i *instance) linkPeerAllowed(c net.Conn) bool {
	host, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	i.mu.Lock()
	ctr := i.ctr
	i.mu.Unlock()
	al, ok := ctr.(AddrLister)
	if !ok || ctr == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, a := range al.IPs(ctx) {
		if p := net.ParseIP(a); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}
