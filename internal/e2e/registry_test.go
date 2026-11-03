package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/regproxy"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// TestRegistry pushes an image the way `docker push` does (through the localhost proxy with an
// API key) and pulls it the way an agent's Docker does (through the proxy with a node token).
func TestRegistry(t *testing.T) {
	cl := newCluster(t)
	push, err := regproxy.Start(cl.url, cl.key, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer push.Close()
	base := "http://" + push.Addr

	do := func(method, url string, body []byte, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, bytes.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	expect := func(resp *http.Response, code int, what string) *http.Response {
		t.Helper()
		if resp.StatusCode != code {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: got %d want %d: %s", what, resp.StatusCode, code, b)
		}
		return resp
	}

	expect(do("GET", base+"/v2/", nil, nil), 200, "ping")

	// Chunked upload of a 3 MB layer: POST, PATCH x2, PUT ?digest=.
	layer := bytes.Repeat([]byte("ogm-layer-"), 300_000)
	ld := digestOf(layer)
	resp := expect(do("POST", base+"/v2/default/myapp/blobs/uploads/", nil, nil), 202, "start upload")
	loc := resp.Header.Get("Location")
	resp = expect(do("PATCH", base+loc, layer[:1_000_000], nil), 202, "patch 1")
	loc = resp.Header.Get("Location")
	resp = expect(do("PATCH", base+loc, layer[1_000_000:], nil), 202, "patch 2")
	loc = resp.Header.Get("Location")
	expect(do("PUT", base+loc+"?digest="+ld, nil, nil), 201, "finish upload")

	// Monolithic upload of the config blob.
	cfg := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	cd := digestOf(cfg)
	expect(do("POST", base+"/v2/default/myapp/blobs/uploads/?digest="+cd, cfg, nil), 201, "monolithic upload")
	expect(do("HEAD", base+"/v2/default/myapp/blobs/"+cd, nil, nil), 200, "head blob")

	// Corrupt upload is rejected.
	resp = expect(do("POST", base+"/v2/default/myapp/blobs/uploads/", nil, nil), 202, "start bad upload")
	expect(do("PUT", base+resp.Header.Get("Location")+"?digest="+ld, []byte("nope"), nil), 400, "digest mismatch")

	// Manifest referencing a missing blob is rejected; a complete one is accepted.
	mt := "application/vnd.oci.image.manifest.v1+json"
	bad, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mt,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": digestOf([]byte("missing")), "size": 7}}})
	expect(do("PUT", base+"/v2/default/myapp/manifests/v1", bad, map[string]string{"Content-Type": mt}), 400, "manifest with missing blob")
	man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mt,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)},
		"layers": []map[string]any{{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": ld, "size": len(layer)}}})
	resp = expect(do("PUT", base+"/v2/default/myapp/manifests/v1", man, map[string]string{"Content-Type": mt}), 201, "put manifest")
	if resp.Header.Get("Docker-Content-Digest") != digestOf(man) {
		t.Fatal("manifest digest header mismatch")
	}

	// Pull as a node, through its own proxy.
	tok, err := cl.c.CreateNode(cl.ctx, "puller")
	if err != nil {
		t.Fatal(err)
	}
	pull, err := regproxy.Start(cl.url, tok.Token, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pull.Close()
	if got := pull.Rewrite("registry.ogm/default/myapp:v1"); got != pull.Addr+"/default/myapp:v1" {
		t.Fatalf("rewrite: %s", got)
	}
	pbase := "http://" + pull.Addr
	resp = expect(do("GET", pbase+"/v2/default/myapp/manifests/v1", nil, map[string]string{"Accept": mt}), 200, "node pulls manifest")
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, man) || resp.Header.Get("Content-Type") != mt {
		t.Fatalf("manifest roundtrip mismatch (%s)", resp.Header.Get("Content-Type"))
	}
	resp = expect(do("GET", pbase+"/v2/default/myapp/blobs/"+ld, nil, nil), 200, "node pulls layer")
	got, _ = io.ReadAll(resp.Body)
	if digestOf(got) != ld {
		t.Fatal("layer corrupted in transit")
	}
	// Range request (resumed pull).
	resp = expect(do("GET", pbase+"/v2/default/myapp/blobs/"+ld, nil, map[string]string{"Range": "bytes=10-19"}), 206, "range")
	got, _ = io.ReadAll(resp.Body)
	if string(got) != "ogm-layer-" {
		t.Fatalf("range body %q", got)
	}
	// Nodes are read-only.
	expect(do("POST", pbase+"/v2/default/myapp/blobs/uploads/", nil, nil), 403, "node push denied")

	// Other members can't touch projects they don't belong to.
	var inv api.Invite
	if err := cl.c.Do(cl.ctx, "POST", "/v1/invites", map[string]any{"uses": 1}, &inv); err != nil {
		t.Fatal(err)
	}
	var mallory api.User
	if err := cl.c.Do(cl.ctx, "POST", "/v1/join", map[string]any{"code": inv.Code, "name": "mallory"}, &mallory); err != nil {
		t.Fatal(err)
	}
	mp, _ := regproxy.Start(cl.url, mallory.APIKey, 0)
	defer mp.Close()
	if err := cl.c.Do(cl.ctx, "POST", "/v1/projects", map[string]any{"name": "private"}, nil); err != nil {
		t.Fatal(err)
	}
	expect(do("POST", base+"/v2/private/secret/blobs/uploads/?digest="+cd, cfg, nil), 201, "admin pushes to private")
	expect(do("GET", "http://"+mp.Addr+"/v2/private/secret/blobs/"+cd, nil, nil), 403, "non-member pull")
	expect(do("POST", "http://"+mp.Addr+"/v2/private/secret/blobs/uploads/", nil, nil), 403, "non-member push")
	expect(do("POST", "http://"+mp.Addr+"/v2/mallory/tool/blobs/uploads/", nil, nil), 202, "member pushes to own project")

	// Unauthenticated access gets a Basic challenge (what `docker login` expects).
	resp = expect(do("GET", cl.url+"/v2/", nil, nil), 401, "anonymous")
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing auth challenge")
	}
	// Tags and catalog.
	resp = expect(do("GET", base+"/v2/default/myapp/tags/list", nil, nil), 200, "tags")
	var tags struct{ Tags []string }
	_ = json.NewDecoder(resp.Body).Decode(&tags)
	if fmt.Sprint(tags.Tags) != "[v1]" {
		t.Fatalf("tags %v", tags.Tags)
	}
}

// TestRegistryMultiArch: x86 and ARM machines push the same tag with --multiarch and the tag
// becomes an index of both; re-pushing one arch replaces only that entry.
func TestRegistryMultiArch(t *testing.T) {
	cl := newCluster(t)
	p, err := regproxy.Start(cl.url, cl.key, 0, http.Header{"Ogm-Merge-Platforms": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	base := "http://" + p.Addr
	put := func(method, path string, body []byte, ct string) *http.Response {
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode >= 300 && method != "GET" {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
		}
		return resp
	}
	pushArch := func(arch, salt string) string {
		cfg := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","salt":%q}`, arch, salt))
		cd := digestOf(cfg)
		put("POST", "/v2/default/app/blobs/uploads/?digest="+cd, cfg, "")
		mt := "application/vnd.oci.image.manifest.v1+json"
		man, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mt,
			"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(cfg)}, "layers": []any{}})
		put("PUT", "/v2/default/app/manifests/v1", man, mt)
		return digestOf(man)
	}
	platforms := func() map[string]string {
		req, _ := http.NewRequest("GET", base+"/v2/default/app/manifests/v1", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var idx struct {
			Manifests []struct {
				Digest   string
				Platform struct{ Architecture string }
			}
		}
		_ = json.NewDecoder(resp.Body).Decode(&idx)
		out := map[string]string{}
		for _, m := range idx.Manifests {
			out[m.Platform.Architecture] = m.Digest
		}
		return out
	}
	amd := pushArch("amd64", "1")
	arm := pushArch("arm64", "1")
	if got := platforms(); got["amd64"] != amd || got["arm64"] != arm || len(got) != 2 {
		t.Fatalf("after two pushes: %v", got)
	}
	amd2 := pushArch("amd64", "2")
	if got := platforms(); got["amd64"] != amd2 || got["arm64"] != arm || len(got) != 2 {
		t.Fatalf("re-push should replace only amd64: %v", got)
	}
}
