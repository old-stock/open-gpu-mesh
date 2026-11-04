package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/proto"
)

// VolumeArchiver is implemented by runtimes that can export/import a volume's contents
// as a tar stream (paths relative to the volume root).
type VolumeArchiver interface {
	ExportVolume(ctx context.Context, image, source string, w *tar.Writer, prefix string) error
	ImportVolume(ctx context.Context, image, source string, r io.Reader) error
}

// handleVolumeOp runs a snapshot ("snapshot") or restore ("restore") and reports back.
func (a *Agent) handleVolumeOp(op string, v proto.VolumeOp) {
	ctx, cancel := context.WithTimeout(a.ctx, 6*time.Hour)
	defer cancel()
	done := proto.VolumeOpDone{ID: v.ID, Op: op}
	var err error
	if op == "snapshot" {
		done.Bytes, err = a.snapshotVolumes(ctx, v)
	} else {
		done.Bytes, err = a.restoreVolumes(ctx, v)
	}
	if err != nil {
		done.Error = err.Error()
		a.log.Warn("volume "+op+" failed", "id", v.ID, "err", err)
	} else {
		a.log.Info("volume "+op+" done", "id", v.ID, "bytes", done.Bytes)
	}
	a.send(proto.TypeVolumeOpDone, done)
}

func (a *Agent) hubURL(rel string) string { return strings.TrimRight(a.cfg.Server, "/") + rel }

func (a *Agent) ensureImage(ctx context.Context, image string, auth *api.RegistryAuth) error {
	image = a.reg.Rewrite(image)
	if a.rt.HasImage(ctx, image) {
		return nil
	}
	return a.rt.Pull(ctx, image, auth, func(float64) {})
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (a *Agent) snapshotVolumes(ctx context.Context, v proto.VolumeOp) (int64, error) {
	va, ok := a.rt.(VolumeArchiver)
	if !ok {
		return 0, errors.New("runtime does not support volume snapshots")
	}
	if err := a.ensureImage(ctx, v.Image, v.Auth); err != nil {
		return 0, fmt.Errorf("helper image: %w", err)
	}
	pr, pw := io.Pipe()
	cw := &countingWriter{w: pw}
	go func() {
		gz := gzip.NewWriter(cw)
		tw := tar.NewWriter(gz)
		var err error
		for _, vol := range v.Volumes {
			if err = va.ExportVolume(ctx, a.reg.Rewrite(v.Image), vol.Source, tw, vol.Name); err != nil {
				break
			}
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = gz.Close()
		}
		pw.CloseWithError(err)
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, a.hubURL(v.URL), pr)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		pr.CloseWithError(err)
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("upload: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return cw.n, nil
}

func (a *Agent) restoreVolumes(ctx context.Context, v proto.VolumeOp) (int64, error) {
	va, ok := a.rt.(VolumeArchiver)
	if !ok {
		return 0, errors.New("runtime does not support volume restore")
	}
	if err := a.ensureImage(ctx, v.Image, v.Auth); err != nil {
		return 0, fmt.Errorf("helper image: %w", err)
	}
	if vr, ok := a.rt.(VolumeRemover); ok {
		var names []string
		for _, vol := range v.Volumes {
			names = append(names, vol.Source)
		}
		if err := vr.RemoveVolumes(ctx, names); err != nil {
			return 0, fmt.Errorf("clear volumes: %w", err)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.hubURL(v.URL), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	cr := &countingReader{r: resp.Body}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return 0, err
	}
	// Split the combined archive into one stream per volume.
	byName := map[string]proto.VolumeMount{}
	for _, vol := range v.Volumes {
		byName[vol.Name] = vol
	}
	type sink struct {
		pw   *io.PipeWriter
		tw   *tar.Writer
		done chan error
	}
	sinks := map[string]*sink{}
	open := func(vol proto.VolumeMount) *sink {
		if s, ok := sinks[vol.Name]; ok {
			return s
		}
		pr, pw := io.Pipe()
		s := &sink{pw: pw, tw: tar.NewWriter(pw), done: make(chan error, 1)}
		go func() {
			err := va.ImportVolume(ctx, a.reg.Rewrite(v.Image), vol.Source, pr)
			pr.CloseWithError(err)
			s.done <- err
		}()
		sinks[vol.Name] = s
		return s
	}
	// Make sure every volume exists even if the snapshot had no entries for it.
	for _, vol := range v.Volumes {
		open(vol)
	}
	tr := tar.NewReader(gz)
	var rerr error
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rerr = err
			break
		}
		top, rest, _ := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		vol, ok := byName[top]
		if !ok || rest == "" {
			continue
		}
		s := open(vol)
		h.Name = rest
		if err := s.tw.WriteHeader(h); err != nil {
			rerr = err
			break
		}
		if _, err := io.Copy(s.tw, tr); err != nil {
			rerr = err
			break
		}
	}
	for _, s := range sinks {
		_ = s.tw.Close()
		s.pw.CloseWithError(rerr)
		if err := <-s.done; err != nil && rerr == nil {
			rerr = err
		}
	}
	return cr.n, rerr
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---------- Docker ----------

func (d *DockerRuntime) helperContainer(ctx context.Context, image, source string) (string, func(), error) {
	body := map[string]any{
		"Image": image, "Entrypoint": []string{"/ogm-volume-helper"}, "Labels": map[string]string{dockerLabel: "1", "dev.ogm.helper": "volume"},
		"HostConfig": map[string]any{"Mounts": []map[string]any{{"Type": "volume", "Source": source, "Target": "/ogmvol",
			"VolumeOptions": map[string]any{"Labels": map[string]string{dockerLabel: "1"}}}}},
	}
	resp, err := d.do(ctx, http.MethodPost, "/containers/create", body, nil)
	if err != nil {
		return "", nil, err
	}
	var cr struct {
		ID string `json:"Id"`
	}
	err = decodeJSON(resp.Body, &cr)
	resp.Body.Close()
	if err != nil || cr.ID == "" {
		return "", nil, fmt.Errorf("create helper container: %v", err)
	}
	cleanup := func() {
		if r, err := d.do(context.Background(), http.MethodDelete, "/containers/"+cr.ID+"?force=1", nil, nil); err == nil {
			r.Body.Close()
		}
	}
	return cr.ID, cleanup, nil
}

// ExportVolume implements VolumeArchiver via the archive endpoint of a never-started
// helper container that mounts the volume.
func (d *DockerRuntime) ExportVolume(ctx context.Context, image, source string, tw *tar.Writer, prefix string) error {
	id, cleanup, err := d.helperContainer(ctx, image, source)
	if err != nil {
		return err
	}
	defer cleanup()
	resp, err := d.do(ctx, http.MethodGet, "/containers/"+id+"/archive?path="+url.QueryEscape("/ogmvol/."), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Entries come as "./x" or "ogmvol/x" depending on the daemon version.
		name := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), "ogmvol/")
		if name == "" || name == "." || name == "ogmvol" {
			name = ""
		}
		h.Name = prefix + "/" + name
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// ImportVolume implements VolumeArchiver.
func (d *DockerRuntime) ImportVolume(ctx context.Context, image, source string, r io.Reader) error {
	id, cleanup, err := d.helperContainer(ctx, image, source)
	if err != nil {
		return err
	}
	defer cleanup()
	d.init()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://docker/v1.43/containers/"+id+"/archive?path=%2Fogmvol", r)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("docker import: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ---------- process runtime ----------

// ExportVolume implements VolumeArchiver for directory volumes.
func (p *ProcessRuntime) ExportVolume(ctx context.Context, _ string, source string, tw *tar.Writer, prefix string) error {
	root := filepath.Join(p.volumeDir(), source)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || ctx.Err() != nil {
			return errors.Join(err, ctx.Err())
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			rel = ""
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		h.Name = prefix + "/" + filepath.ToSlash(rel)
		if fi.IsDir() && rel != "" {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
}

// ImportVolume implements VolumeArchiver for directory volumes.
func (p *ProcessRuntime) ImportVolume(_ context.Context, _ string, source string, r io.Reader) error {
	root := filepath.Join(p.volumeDir(), source)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		dst := filepath.Join(root, filepath.FromSlash(h.Name))
		if dst != root && !strings.HasPrefix(dst, root+string(os.PathSeparator)) {
			continue // path traversal
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, os.FileMode(h.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(dst), 0o700)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
