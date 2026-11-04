package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/store"
)

const secretsSchema = `
CREATE TABLE IF NOT EXISTS secrets (project TEXT NOT NULL, name TEXT NOT NULL, value TEXT NOT NULL,
  created INTEGER NOT NULL, updated INTEGER NOT NULL, PRIMARY KEY(project, name));`

const maxSecretBytes = 64 << 10

// initSecrets loads (or creates) the AES-256 key that encrypts secrets at rest. It lives in
// its own file next to the database, so a copy of ogm.db alone doesn't reveal secrets.
func (s *Server) initSecrets() error {
	if _, err := s.st.DB.Exec(secretsSchema); err != nil {
		return err
	}
	if s.cfg.DataDir == "" {
		s.secretKey = make([]byte, 32)
		_, _ = rand.Read(s.secretKey)
		return nil
	}
	p := filepath.Join(s.cfg.DataDir, "secrets.key")
	if b, err := os.ReadFile(p); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return fmt.Errorf("%s is corrupt", p)
		}
		s.secretKey = key
		return nil
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return err
	}
	s.secretKey = key
	return nil
}

func (s *Server) sealSecret(plain []byte) (string, error) {
	blk, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func (s *Server) openSecret(sealed string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	if len(b) < gcm.NonceSize() {
		return nil, errors.New("sealed secret too short")
	}
	return gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
}

// secretValue returns a project secret's plaintext.
func (s *Server) secretValue(project, name string) (string, error) {
	var sealed string
	if err := s.st.DB.QueryRow(`SELECT value FROM secrets WHERE project=? AND name=?`, project, name).Scan(&sealed); err != nil {
		return "", fmt.Errorf("secret %q not found in project %s", name, project)
	}
	b, err := s.openSecret(sealed)
	if err != nil {
		return "", fmt.Errorf("secret %q can't be decrypted (secrets.key changed?)", name)
	}
	return string(b), nil
}

// expandSecrets replaces {{secret:NAME}} placeholders in v.
func (s *Server) expandSecrets(project, v string) (string, error) {
	for _, ref := range api.ParseRefs(v) {
		if ref.Kind != "secret" {
			continue
		}
		val, err := s.secretValue(project, ref.Name)
		if err != nil {
			return "", err
		}
		v = strings.ReplaceAll(v, ref.Raw, val)
	}
	return v, nil
}

func (s *Server) registerSecretRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/projects/{project}/secrets", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		rows, err := s.st.DB.Query(`SELECT name, created, updated FROM secrets WHERE project=? ORDER BY name`, p)
		if err != nil {
			storeErr(w, err, "secrets")
			return
		}
		defer rows.Close()
		out := []api.Secret{}
		for rows.Next() {
			var sc api.Secret
			var c, up int64
			if rows.Scan(&sc.Name, &c, &up) == nil {
				sc.CreateTime, sc.UpdateTime = store.T(c), store.T(up)
				out = append(out, sc)
			}
		}
		writeJSON(w, http.StatusOK, api.List[api.Secret]{Items: out})
	}))
	mux.HandleFunc("PUT /v1/projects/{project}/secrets/{name}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		name := r.PathValue("name")
		if !api.ValidSecretName(name) {
			problem(w, http.StatusBadRequest, "invalid secret name", "use letters, digits, _ . - (max 128)")
			return
		}
		var body struct {
			Value *string `json:"value"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Value == nil || len(*body.Value) > maxSecretBytes {
			problem(w, http.StatusBadRequest, "invalid secret", `body must be {"value": "..."} up to 64 KiB`)
			return
		}
		sealed, err := s.sealSecret([]byte(*body.Value))
		if err != nil {
			storeErr(w, err, "secret")
			return
		}
		now := store.Now()
		if _, err := s.st.DB.Exec(`INSERT INTO secrets(project,name,value,created,updated) VALUES(?,?,?,?,?)
			ON CONFLICT(project,name) DO UPDATE SET value=excluded.value, updated=excluded.updated`, p, name, sealed, now, now); err != nil {
			storeErr(w, err, "secret")
			return
		}
		s.st.AddEvent(p, "", "", "", "secret.set", "Secret "+name+" set by "+u.Name)
		s.Poke() // instances waiting for this secret can now be placed
		writeJSON(w, http.StatusOK, api.Secret{Name: name, UpdateTime: store.T(now)})
	}))
	mux.HandleFunc("DELETE /v1/projects/{project}/secrets/{name}", s.projected(func(w http.ResponseWriter, r *http.Request, u *store.UserRow, p string) {
		res, err := s.st.DB.Exec(`DELETE FROM secrets WHERE project=? AND name=?`, p, r.PathValue("name"))
		if err != nil {
			storeErr(w, err, "secret")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			problem(w, http.StatusNotFound, "not found", "no such secret")
			return
		}
		s.st.AddEvent(p, "", "", "", "secret.deleted", "Secret "+r.PathValue("name")+" deleted by "+u.Name)
		w.WriteHeader(http.StatusNoContent)
	}))
}
