package runner

import (
	"reflect"
	"testing"
)

func TestDockerDNS(t *testing.T) {
	cfg := Config{Namespace: "mvp-test", Image: "worker:test", Network: "test", UDPBase: 51000, DockerDNS: []string{"192.0.2.53", "2001:db8::53"}}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := r.withDockerDNS([]string{"run", "--read-only", cfg.Image})
	want := []string{"run", "--read-only", "--dns", "192.0.2.53", "--dns", "2001:db8::53", cfg.Image}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DNS must be Docker options before image: %v", got)
	}
	cfg.DockerDNS = []string{"resolver.example.org"}
	if _, err := New(cfg); err == nil {
		t.Fatal("hostname accepted as DNS IP")
	}
}
