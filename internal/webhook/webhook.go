// Package webhook implements Standard Webhooks (svix-compatible) signing and verification,
// and EdDSA JWTs for instance identity.
package webhook

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sign returns the `webhook-signature` header value for a payload.
// secret is base64 (an optional "whsec_" prefix is accepted).
func Sign(secret, msgID string, ts time.Time, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return "", fmt.Errorf("decode secret: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", msgID, ts.Unix())
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Headers returns the three Standard Webhooks headers.
func Headers(secret, msgID string, ts time.Time, body []byte) (http.Header, error) {
	sig, err := Sign(secret, msgID, ts, body)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("webhook-id", msgID)
	h.Set("webhook-timestamp", strconv.FormatInt(ts.Unix(), 10))
	h.Set("webhook-signature", sig)
	h.Set("Content-Type", "application/json")
	return h, nil
}

// Verify checks a webhook request's signature (tolerance 5 minutes).
func Verify(secret string, h http.Header, body []byte) error {
	id, tsStr, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || tsStr == "" || sigs == "" {
		return errors.New("missing webhook headers")
	}
	tsi, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	ts := time.Unix(tsi, 0)
	if d := time.Since(ts); d > 5*time.Minute || d < -5*time.Minute {
		return errors.New("timestamp outside tolerance")
	}
	want, err := Sign(secret, id, ts, body)
	if err != nil {
		return err
	}
	for _, s := range strings.Fields(sigs) {
		if subtle.ConstantTimeCompare([]byte(s), []byte(want)) == 1 {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

// ---------- JWT (EdDSA) ----------

var b64 = base64.RawURLEncoding

// SignJWT creates a compact EdDSA JWT.
func SignJWT(priv ed25519.PrivateKey, kid string, claims map[string]any) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(hdr) + "." + b64.EncodeToString(body)
	sig := ed25519.Sign(priv, []byte(signing))
	return signing + "." + b64.EncodeToString(sig), nil
}

// VerifyJWT verifies signature and exp, returning claims.
func VerifyJWT(pub ed25519.PublicKey, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed jwt")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad signature encoding")
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("invalid signature")
	}
	body, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, err
	}
	if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp) {
		return nil, errors.New("token expired")
	}
	return claims, nil
}

// JWK renders an Ed25519 public key as a JWK.
func JWK(pub ed25519.PublicKey, kid string) map[string]string {
	return map[string]string{"kty": "OKP", "crv": "Ed25519", "x": b64.EncodeToString(pub), "kid": kid, "alg": "EdDSA", "use": "sig"}
}
