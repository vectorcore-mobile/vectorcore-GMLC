package delivery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SignatureHeader is the HTTP header carrying Sign's output, so the
// receiving endpoint can verify a delivery actually came from this GMLC and
// wasn't forged/tampered with in transit.
const SignatureHeader = "X-GMLC-Signature"

// Sign computes the HMAC-SHA256 signature of payload under secret, in the
// "sha256=<hex>" form conventional for webhook signature headers (e.g.
// GitHub, Stripe), so receiving endpoints can use off-the-shelf verification
// code rather than a bespoke format.
func Sign(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
