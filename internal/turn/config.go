package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"github.com/pion/webrtc/v4"
	"strconv"
	"time"
)

// Credentials follow coturn's shared-secret REST authentication mechanism.
func Credentials(url, secret, subject string, now time.Time) []webrtc.ICEServer {
	if url == "" || secret == "" {
		return nil
	}
	username := strconv.FormatInt(now.Add(12*time.Hour).Unix(), 10) + ":" + subject
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(username))
	return []webrtc.ICEServer{{URLs: []string{url}, Username: username, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))}}
}
