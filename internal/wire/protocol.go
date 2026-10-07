// Package wire contains the private gateway/runner/worker protocol.
package wire

import "github.com/pion/webrtc/v4"

type Start struct {
	ID       string `json:"id"`
	Template string `json:"template"`
	Control  bool   `json:"control"`
	Print    bool   `json:"print"`
}
type Offer struct {
	AuthExpiresAt int64                     `json:"auth_expires_at,omitempty"`
	PolicyVersion int64                     `json:"policy_version"`
	Generation    string                    `json:"generation"`
	SDP           webrtc.SessionDescription `json:"sdp"`
	Control       bool                      `json:"control"`
	Print         bool                      `json:"print"`
}
type Lease struct {
	PolicyVersion int64  `json:"policy_version"`
	Generation    string `json:"generation"`
	Control       bool   `json:"control"`
	Print         bool   `json:"print"`
}
type Config struct {
	ICEServers         []webrtc.ICEServer `json:"iceServers"`
	ICETransportPolicy string             `json:"iceTransportPolicy"`
}
type Input struct {
	Version  int     `json:"v"`
	Type     string  `json:"type"`
	Sequence uint64  `json:"seq"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Code     string  `json:"code"`
	Down     bool    `json:"down"`
	Button   int     `json:"button"`
	Delta    int     `json:"delta"`
}
