package runner

import (
	"context"
	"encoding/json"
	"io"
	"mvp1/internal/egress"
	"mvp1/internal/wire"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitNetworkPolicyHasNoAppEscape(t *testing.T) {
	k := &kubernetes{namespace: "test"}
	id := strings.Repeat("a", 64)
	app := k.splitPolicy(id, "app")
	spec := app["spec"].(map[string]any)
	selector := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]string)
	if selector["mvp1.part"] != "app" || selector["mvp1.session"] != id[:56] {
		t.Fatal("app policy not scoped to session")
	}
	raw, _ := json.Marshal(spec["egress"])
	if strings.Contains(string(raw), "ipBlock") || strings.Contains(string(raw), "namespaceSelector") || strings.Contains(string(raw), "53") {
		t.Fatalf("app egress escapes pair: %s", raw)
	}
	for _, port := range []string{`"port":8082`, `"port":8631`} {
		if !strings.Contains(string(raw), port) {
			t.Fatalf("missing app egress port: %s", raw)
		}
	}
	if strings.Contains(string(raw), `"port":8081`) {
		t.Fatal("app can reach worker control API")
	}
	other, _ := json.Marshal(k.splitPolicy(strings.Repeat("b", 64), "app"))
	if strings.Contains(string(other), id[:56]) {
		t.Fatal("session selector reused")
	}
}

func TestSplitKubernetesCreatesPoliciesBeforePodsAndCleansPair(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	objects := map[string]map[string]any{}
	policies := 0
	pods := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			var obj map[string]any
			if json.NewDecoder(r.Body).Decode(&obj) != nil {
				t.Error("invalid JSON")
				w.WriteHeader(400)
				return
			}
			meta := obj["metadata"].(map[string]any)
			name := meta["name"].(string)
			meta["uid"] = "uid-" + name
			if obj["kind"] == "NetworkPolicy" {
				policies++
			} else {
				if policies != 2 {
					t.Error("Pod created before both policies")
				}
				pods++
				labels := meta["labels"].(map[string]any)
				spec := obj["spec"].(map[string]any)
				if spec["automountServiceAccountToken"] != false {
					t.Error("service account exposed")
				}
				if labels["mvp1.part"] == "app" {
					raw, _ := json.Marshal(spec)
					for _, secret := range []string{"MVP_WORKER_TOKEN", "MVP_ICE_SERVERS", "worker-secret"} {
						if strings.Contains(string(raw), secret) {
							t.Errorf("app leaked %s", secret)
						}
					}
					if labels["app.kubernetes.io/component"] != "isolated-app" {
						t.Error("app matches legacy egress policy")
					}
				}
				obj["status"] = map[string]any{"phase": "Running", "podIP": "10.244.1.2"}
			}
			objects[r.URL.Path+"/"+name] = obj
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(obj)
			return
		}
		obj, ok := objects[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if r.Method == "DELETE" {
			delete(objects, r.URL.Path)
			w.WriteHeader(200)
			return
		}
		json.NewEncoder(w).Encode(obj)
	}))
	defer api.Close()
	k := &kubernetes{namespace: "test", base: api.URL, tokenFile: tokenFile, client: api.Client(), image: "test-image"}
	attached := false
	r := &Runner{kube: k, config: Config{DeniedCIDRs: "10.244.0.0/16"}, http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/attach" || req.Header.Get("Authorization") != "Bearer worker-secret" {
			t.Errorf("bad attachment: %s", req.URL)
		}
		var attachment map[string]string
		json.NewDecoder(req.Body).Decode(&attachment)
		if attachment["Address"] != "http://10.244.1.2:8090" || len(attachment["Token"]) != 64 {
			t.Error("invalid agent attachment")
		}
		attached = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
	})}}
	id := strings.Repeat("c", 64)
	_, err := r.startSplitKube(context.Background(), wire.Start{ID: id, Template: "firefox", StartURL: "https://example.org", Access: &egress.Policy{Domains: []string{"example.org"}, Ports: []uint16{443}}}, worker{token: "worker-secret"}, "[]")
	if err != nil || !attached || pods != 2 || len(objects) != 4 {
		t.Fatalf("pair launch failed: %v attached=%v pods=%d objects=%d", err, attached, pods, len(objects))
	}
	if err = k.stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatal("pair resources leaked")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A missing NetworkPolicy API must fail before creating any Pod.
func TestSplitFailsClosedWithoutNetworkPolicyAPI(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "POST" || !strings.Contains(r.URL.Path, "networkpolicies") {
			t.Errorf("Pod started without policies: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	k := &kubernetes{client: srv.Client(), base: srv.URL, namespace: "test", tokenFile: tokenFile, image: "test:image"}
	r := &Runner{kube: k}
	_, err := r.startSplitKube(context.Background(), wire.Start{ID: strings.Repeat("a", 64)}, worker{}, "[]")
	if err == nil || requests != 1 {
		t.Fatalf("missing policy API accepted: err=%v calls=%d", err, requests)
	}
}
