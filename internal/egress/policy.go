// Package egress defines the destination rules of a published application.
package egress

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Policy is an allow list. An empty list grants no access. Domains and CIDRs
// are alternatives; ports apply to both. Domains use ASCII (IDN punycode).
type Policy struct {
	Domains []string `json:"domains"`
	CIDRs   []string `json:"cidrs"`
	Ports   []uint16 `json:"ports"`
}

func domain(s string) bool {
	if s == "" || len(s) > 253 || strings.HasSuffix(s, ".") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	_, err := netip.ParseAddr(s)
	return err != nil
}

func (p Policy) Validate() error {
	if len(p.Domains)+len(p.CIDRs) > 128 || len(p.Ports) > 32 {
		return fmt.Errorf("too many access rules")
	}
	for _, d := range p.Domains {
		if !domain(strings.TrimPrefix(d, "*.")) {
			return fmt.Errorf("invalid domain rule: %q", d)
		}
	}
	for _, c := range p.CIDRs {
		prefix, err := netip.ParsePrefix(c)
		if err != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return fmt.Errorf("invalid canonical CIDR: %q", c)
		}
	}
	for _, port := range p.Ports {
		if port == 0 {
			return fmt.Errorf("port must be nonzero")
		}
	}
	return nil
}

func (p Policy) AllowsPort(port uint16) bool {
	for _, allowed := range p.Ports {
		if port == allowed {
			return true
		}
	}
	return false
}

func (p Policy) AllowsDomain(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !domain(host) {
		return false
	}
	for _, rule := range p.Domains {
		if strings.HasPrefix(rule, "*.") {
			if strings.HasSuffix(host, rule[1:]) {
				return true
			}
		} else if host == rule {
			return true
		}
	}
	return false
}

// AllowsIP evaluates an IP destination. DNS names must additionally be checked
// by the proxy; this method alone is not protection against DNS rebinding.
func (p Policy) AllowsIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" {
		return false
	}
	for _, rule := range p.CIDRs {
		prefix, err := netip.ParsePrefix(rule)
		if err == nil && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// ValidateStartURL rejects ambiguous destinations and requires a matching
// explicit domain or literal IP rule. It does not perform network resolution.
func (p Policy) ValidateStartURL(raw string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if len(raw) > 4096 {
		return fmt.Errorf("start URL is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("start URL must be an HTTP(S) URL without credentials")
	}
	port := uint64(443)
	if u.Scheme == "http" {
		port = 80
	}
	if u.Port() != "" {
		port, err = strconv.ParseUint(u.Port(), 10, 16)
		if err != nil {
			return fmt.Errorf("invalid port")
		}
	}
	if strings.HasSuffix(u.Host, ":") || !p.AllowsPort(uint16(port)) {
		return fmt.Errorf("start URL port is not allowed")
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if p.AllowsIP(ip) {
			return nil
		}
	} else if p.AllowsDomain(host) {
		return nil
	}
	return fmt.Errorf("start URL destination is not allowed")
}
