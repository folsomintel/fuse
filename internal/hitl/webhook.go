package hitl

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// HeaderSignature carries the webhook signature: "t=<unix>,v1=<hex>", where
// v1 is HMAC-SHA256 over "<t>.<body>" keyed by the named secret.
const HeaderSignature = "Fuse-Signature"

// Notification is the whole body of a webhook delivery. it is content-free
// by design: a receiver learns that something needs a look and where, never
// what. the human fetches the content from the api with their own key.
type Notification struct {
	Event          AuditEvent `json:"event"`
	InterventionID string     `json:"intervention_id,omitempty"`
	EnvID          string     `json:"env_id"`
	Gate           string     `json:"gate"`
	On             string     `json:"on"`
	Deadline       *time.Time `json:"deadline,omitempty"`
	URL            string     `json:"url,omitempty"`
}

// Sign returns the Fuse-Signature header value for body at time ts.
func Sign(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return "t=" + t + ",v1=" + mac(secret, t, body)
}

func mac(secret []byte, t string, body []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// ErrBadSignature is a webhook signature that does not verify.
var ErrBadSignature = errors.New("webhook signature does not verify")

// VerifySignature checks a Fuse-Signature header against body. tolerance
// bounds how old the timestamp may be, so a captured delivery cannot be
// replayed later; zero skips the age check.
func VerifySignature(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	if t == "" || v1 == "" {
		return fmt.Errorf("%w: header must be t=<unix>,v1=<hex>", ErrBadSignature)
	}
	if !hmac.Equal([]byte(mac(secret, t, body)), []byte(v1)) {
		return ErrBadSignature
	}
	if tolerance > 0 {
		sec, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: bad timestamp", ErrBadSignature)
		}
		if age := now.Sub(time.Unix(sec, 0)); age > tolerance || age < -tolerance {
			return fmt.Errorf("%w: timestamp outside tolerance", ErrBadSignature)
		}
	}
	return nil
}
