package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

func TestSignatureBindingAndFreshness(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	u, _ := url.Parse("https://EXAMPLE.com:443/events?q=a%20b")
	now := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	body := []byte(`{"data":{"content":"日本語 <@123>"},"delivery_id":"dl_a"}`)
	h := Sign(key, "POST", u, body, now)
	if err := Verify(pub, "POST", u, body, h, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, target string
		body                 []byte
		at                   time.Time
	}{
		{"body", "POST", u.String(), []byte(`{"delivery_id":"other"}`), now},
		{"method", "DELETE", u.String(), body, now},
		{"path", "POST", "https://example.com/other?q=a%20b", body, now},
		{"host", "POST", "https://other.example.com/events?q=a%20b", body, now},
		{"expired", "POST", u.String(), body, now.Add(301 * time.Second)},
		{"future", "POST", u.String(), body, now.Add(-301 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, _ := url.Parse(tc.target)
			if Verify(pub, tc.method, target, tc.body, h, tc.at) == nil {
				t.Fatal("accepted invalid signature")
			}
		})
	}
	fresh := Sign(key, "POST", u, body, now.Add(time.Hour))
	if err := Verify(pub, "POST", u, body, fresh, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopePreservesDataAndEmptyAttachmentSelection(t *testing.T) {
	raw := json.RawMessage(`{"timestamp":"old","attachments":[{"id":"1"},{"id":"2"}],"content":"<@123>"}`)
	empty := []json.RawMessage{}
	b, err := JSON(Envelope{Data: raw, Context: map[string]any{}, Attachments: &empty})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["attachments"]) != "[]" {
		t.Fatalf("empty selected attachments omitted: %s", b)
	}
	var a, z any
	json.Unmarshal(raw, &a)
	json.Unmarshal(got["data"], &z)
	aa, _ := JSON(a)
	zz, _ := JSON(z)
	if string(aa) != string(zz) {
		t.Fatal("changed original data")
	}
	if _, exists := got["version"]; exists {
		t.Fatal("unexpected schema version")
	}
}

func TestResultCanonicalization(t *testing.T) {
	a := Result{DeliveryID: "dl", DestinationID: "dst", Status: PartialSuccess, Timestamps: Timestamps{WorkerAcceptedAt: "2026-11-12T01:00:00Z", WorkerFinishedAt: "2026-11-12T01:00:01Z"}, Details: json.RawMessage(`{"b":2,"a":1}`)}
	b := a
	b.Details = json.RawMessage(`{"a":1,"b":2}`)
	x, err := CanonicalResult(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := CanonicalResult(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(x) != string(y) {
		t.Fatal("key ordering changed result identity")
	}
}

func TestResultCanonicalizationPreservesNumbers(t *testing.T) {
	r := Result{DeliveryID: "dl", DestinationID: "dst", Status: Success, Timestamps: Timestamps{WorkerAcceptedAt: "2026-11-12T01:00:00Z", WorkerFinishedAt: "2026-11-12T01:00:01Z"}}
	for _, number := range []string{"9007199254740992", "9007199254740993", "18446744073709551615", "0.12345678901234567890123456789", "1e400"} {
		t.Run(number, func(t *testing.T) {
			r.Details = json.RawMessage(`{"nested":[{"n":` + number + `}]}`)
			canonical, err := CanonicalResult(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(canonical, []byte(`"n":`+number)) {
				t.Fatalf("numeric value changed: %s", canonical)
			}
		})
	}
	r.Details = json.RawMessage(`{"n":9007199254740992}`)
	a, _ := CanonicalResult(r)
	r.Details = json.RawMessage(`{"n":9007199254740993}`)
	b, _ := CanonicalResult(r)
	if bytes.Equal(a, b) {
		t.Fatal("distinct integers produced an identical result")
	}
}
