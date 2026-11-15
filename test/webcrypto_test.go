package test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/protocol"
)

func TestWebCryptoInteroperability(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is only needed for the WebCrypto interoperability check")
	}
	seed := bytes.Repeat([]byte{1}, 32)
	key := ed25519.NewKeyFromSeed(seed)
	pub := key.Public().(ed25519.PublicKey)
	target, _ := url.Parse("https://EXAMPLE.com:443/events?q=%E3%83%86%E3%82%B9%E3%83%88")
	now := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	body := []byte(`{"data":{"content":"日本語 <@123>"},"destination_id":"dst_test"}`)
	headers := protocol.Sign(key, "POST", target, body, now)
	flat := map[string]string{}
	for k := range headers {
		flat[k] = headers.Get(k)
	}
	vector := map[string]any{"seed": base64.RawURLEncoding.EncodeToString(seed), "public_key": protocol.EncodePublicKey(pub), "method": "POST", "url": target.String(), "body": string(body), "headers": flat, "now_ms": now.UnixMilli()}
	input, _ := json.Marshal(vector)
	script, _ := filepath.Abs("webcrypto/signatures-check.mjs")
	command := exec.Command(node, script)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("WebCrypto rejected Go signature: %v: %s", err, output)
	}
	var signed map[string]string
	if err = json.Unmarshal(output, &signed); err != nil {
		t.Fatalf("invalid oracle output: %s", output)
	}
	got := make(http.Header)
	for k, v := range signed {
		got.Set(k, v)
	}
	if err = protocol.Verify(pub, "POST", target, body, got, now); err != nil {
		t.Fatalf("Go rejected WebCrypto signature: %v", err)
	}
}
