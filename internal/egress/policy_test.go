package egress

import (
	"net/netip"
	"testing"
)

func TestDomainBoundaries(t *testing.T) {
	p := Policy{Domains: []string{"dion.vc", "*.example.org"}}
	for host, want := range map[string]bool{"dion.vc": true, "DION.VC.": true, "a.dion.vc": false, "evildion.vc": false, "dion.vc.evil": false, "example.org": false, "a.example.org": true, "a.b.example.org": true, "badexample.org": false, "a..example.org": false, "127.0.0.1": false} {
		if got := p.AllowsDomain(host); got != want {
			t.Errorf("%s: got %v want %v", host, got, want)
		}
	}
}
func TestStartURL(t *testing.T) {
	p := Policy{Domains: []string{"ya.ru"}, CIDRs: []string{"10.2.0.0/16", "2001:db8::/32"}, Ports: []uint16{443}}
	for raw, want := range map[string]bool{"https://ya.ru/": true, "https://ya.ru:443/path?q=1": true, "http://ya.ru": false, "https://ya.ru.evil": false, "https://user:pass@ya.ru": false, "file:///etc/passwd": false, "https://10.2.3.4/": true, "https://10.3.3.4/": false, "https://[2001:db8::1]/": true, "https://ya.ru:0": false, "https://ya.ru:": false, "https://ya.ru:65536": false} {
		if got := p.ValidateStartURL(raw) == nil; got != want {
			t.Errorf("%s: got %v want %v", raw, got, want)
		}
	}
}
func TestInvalidRules(t *testing.T) {
	for _, p := range []Policy{{Domains: []string{"https://ya.ru"}}, {Domains: []string{"*."}}, {Domains: []string{"YA.RU"}}, {Domains: []string{"127.0.0.1"}}, {CIDRs: []string{"10.2.3.4/16"}}, {CIDRs: []string{"::ffff:10.0.0.0/104"}}, {Ports: []uint16{0}}} {
		if p.Validate() == nil {
			t.Errorf("accepted invalid policy: %+v", p)
		}
	}
}
func TestEmptyPolicyDenies(t *testing.T) {
	p := Policy{}
	if p.AllowsDomain("ya.ru") || p.AllowsIP(netip.MustParseAddr("10.0.0.1")) || p.AllowsPort(443) {
		t.Fatal("empty policy grants access")
	}
}
