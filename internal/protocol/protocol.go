package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderKeyID     = "X-Gateway-Key-Id"
	HeaderTimestamp = "X-Gateway-Timestamp"
	HeaderSignature = "X-Gateway-Signature"
	Tolerance       = 5 * time.Minute
)

type Timestamps struct {
	EC2ReceivedAt       string `json:"ec2_received_at,omitempty"`
	EC2SentAt           string `json:"ec2_sent_at,omitempty"`
	WorkerAcceptedAt    string `json:"worker_accepted_at,omitempty"`
	WorkerFinishedAt    string `json:"worker_finished_at,omitempty"`
	EC2ResultReceivedAt string `json:"ec2_result_received_at,omitempty"`
}

type Envelope struct {
	DeliveryID    string             `json:"delivery_id"`
	EventID       string             `json:"event_id"`
	TriggerID     string             `json:"trigger_id"`
	DestinationID string             `json:"destination_id"`
	EventType     string             `json:"event_type"`
	Timestamps    Timestamps         `json:"timestamps"`
	Data          json.RawMessage    `json:"data"`
	Context       map[string]any     `json:"context"`
	Attachments   *[]json.RawMessage `json:"attachments,omitempty"`
}

type Acceptance struct {
	DeliveryID string     `json:"delivery_id"`
	Timestamps Timestamps `json:"timestamps"`
}

type ResultStatus string

const (
	Success        ResultStatus = "success"
	PartialSuccess ResultStatus = "partial_success"
	Failure        ResultStatus = "failure"
	Skipped        ResultStatus = "skipped"
)

type Result struct {
	DeliveryID    string          `json:"delivery_id"`
	DestinationID string          `json:"destination_id"`
	Status        ResultStatus    `json:"status"`
	Timestamps    Timestamps      `json:"timestamps"`
	Details       json.RawMessage `json:"result,omitempty"`
}

func (r Result) Validate() error {
	if r.DeliveryID == "" || r.DestinationID == "" {
		return errors.New("delivery_id and destination_id are required")
	}
	switch r.Status {
	case Success, PartialSuccess, Failure, Skipped:
	default:
		return errors.New("invalid final result status")
	}
	for name, value := range map[string]string{"worker_accepted_at": r.Timestamps.WorkerAcceptedAt, "worker_finished_at": r.Timestamps.WorkerFinishedAt} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return fmt.Errorf("%s must be RFC 3339", name)
		}
	}
	a, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerAcceptedAt)
	f, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerFinishedAt)
	if f.Before(a) {
		return errors.New("worker_finished_at precedes worker_accepted_at")
	}
	if len(r.Details) > 0 && !json.Valid(r.Details) {
		return errors.New("invalid result JSON")
	}
	return nil
}

func NewID(prefix string) string { return prefix + "_" + rand.Text() }
func Time(t time.Time) string    { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func JSON(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be 32 bytes encoded as unpadded Base64URL")
	}
	return ed25519.PublicKey(b), nil
}
func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("private key must be a 32-byte seed encoded as unpadded Base64URL")
	}
	return ed25519.NewKeyFromSeed(b), nil
}
func EncodePublicKey(key ed25519.PublicKey) string { return base64.RawURLEncoding.EncodeToString(key) }
func KeyID(key ed25519.PublicKey) string           { h := sha256.Sum256(key); return hex.EncodeToString(h[:16]) }
func Digest(body []byte) string                    { h := sha256.Sum256(body); return hex.EncodeToString(h[:]) }

func Authority(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		host += ":" + port
	}
	return host
}
func Target(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}
func signingInput(keyID, stamp, method string, u *url.URL, body []byte) []byte {
	return []byte(strings.Join([]string{"discord-management-gateway", keyID, stamp, strings.ToUpper(method), Authority(u), Target(u), Digest(body)}, "\n"))
}
func Sign(key ed25519.PrivateKey, method string, u *url.URL, body []byte, now time.Time) http.Header {
	pub := key.Public().(ed25519.PublicKey)
	id := KeyID(pub)
	stamp := strconv.FormatInt(now.Unix(), 10)
	sig := ed25519.Sign(key, signingInput(id, stamp, method, u, body))
	h := make(http.Header)
	h.Set(HeaderKeyID, id)
	h.Set(HeaderTimestamp, stamp)
	h.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(sig))
	h.Set("Content-Type", "application/json")
	return h
}
func Verify(key ed25519.PublicKey, method string, u *url.URL, body []byte, h http.Header, now time.Time) error {
	if len(key) != ed25519.PublicKeySize || h.Get(HeaderKeyID) != KeyID(key) {
		return errors.New("unknown signing key")
	}
	stamp := h.Get(HeaderTimestamp)
	sec, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return errors.New("invalid signature timestamp")
	}
	issued := time.Unix(sec, 0)
	if issued.Before(now.Add(-Tolerance)) || issued.After(now.Add(Tolerance)) {
		return errors.New("signature timestamp outside allowed interval")
	}
	sig, err := base64.RawURLEncoding.DecodeString(h.Get(HeaderSignature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("invalid signature encoding")
	}
	if !ed25519.Verify(key, signingInput(KeyID(key), stamp, method, u, body), sig) {
		return errors.New("invalid signature")
	}
	return nil
}
func RequestURL(r *http.Request) *url.URL {
	u := *r.URL
	if u.Host == "" {
		u.Host = r.Host
	}
	if u.Scheme == "" {
		if r.TLS != nil {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
	}
	return &u
}

// CanonicalResult ignores JSON key ordering and normalizes the two worker times.
func CanonicalResult(r Result) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	a, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerAcceptedAt)
	f, _ := time.Parse(time.RFC3339Nano, r.Timestamps.WorkerFinishedAt)
	r.Timestamps = Timestamps{WorkerAcceptedAt: Time(a), WorkerFinishedAt: Time(f)}
	if len(r.Details) > 0 {
		var v any
		decoder := json.NewDecoder(bytes.NewReader(r.Details))
		decoder.UseNumber()
		if err := decoder.Decode(&v); err != nil {
			return nil, err
		}
		b, err := JSON(v)
		if err != nil {
			return nil, err
		}
		r.Details = b
	}
	return JSON(r)
}
