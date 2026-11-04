package webhook

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestWebhookSignVerify(t *testing.T) {
	secret := "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	body := []byte(`{"hello":"world"}`)
	h, err := Headers(secret, "msg_1", time.Now(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(secret, h, body); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if Verify(secret, h, []byte(`{"hello":"mallory"}`)) == nil {
		t.Fatal("tampered body accepted")
	}
	old, _ := Headers(secret, "msg_2", time.Now().Add(-time.Hour), body)
	if Verify(secret, old, body) == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestJWT(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok, err := SignJWT(priv, "k1", map[string]any{"sub": "inst_1", "exp": time.Now().Add(time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyJWT(pub, tok)
	if err != nil || c["sub"] != "inst_1" {
		t.Fatalf("verify: %v %v", c, err)
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyJWT(otherPub, tok); err == nil {
		t.Fatal("wrong key accepted")
	}
	expired, _ := SignJWT(priv, "k1", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
	if _, err := VerifyJWT(pub, expired); err == nil {
		t.Fatal("expired token accepted")
	}
}

func TestJWTRequiresExp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tok, _ := SignJWT(priv, "k", map[string]any{"sub": "x"})
	if _, err := VerifyJWT(pub, tok); err == nil {
		t.Fatal("token without exp accepted")
	}
}
