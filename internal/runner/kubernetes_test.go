package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestKubernetesLifecycleAndTokenRotation(t *testing.T) {
	id := strings.Repeat("a", 64)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("token-one"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	exists := false
	deleted := false
	expected := "token-one"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("token not reloaded")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		pod := map[string]any{"metadata": map[string]any{"name": podName(id), "uid": "uid-one", "labels": map[string]string{"mvp1.runner": "test", "app.kubernetes.io/component": "session"}}, "status": map[string]string{"phase": "Running", "podIP": "10.1.2.3"}}
		switch r.Method {
		case "POST":
			var v map[string]any
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Error(err)
			}
			spec := v["spec"].(map[string]any)
			if spec["automountServiceAccountToken"] != false || spec["restartPolicy"] != "Never" {
				t.Error("unsafe session pod")
			}
			if spec["hostNetwork"] != nil {
				t.Error("worker must not use host network")
			}
			container := spec["containers"].([]any)[0].(map[string]any)
			sc := container["securityContext"].(map[string]any)
			if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true {
				t.Error("missing restrictions")
			}
			raw, _ := json.Marshal(v)
			if strings.Contains(string(raw), "shared-secret") {
				t.Error("TURN shared secret leaked")
			}
			exists = true
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(pod)
		case "DELETE":
			var v struct {
				Preconditions struct {
					UID string `json:"uid"`
				} `json:"preconditions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			if v.Preconditions.UID != "uid-one" {
				t.Error("missing UID precondition")
			}
			exists = false
			deleted = true
			w.WriteHeader(200)
		case "GET":
			if r.URL.Query().Get("labelSelector") != "" {
				items := []any{}
				if exists {
					items = append(items, pod)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
				return
			}
			if !exists {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(pod)
		}
	}))
	defer server.Close()
	k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: tokenFile, image: "registry/worker:release", pullSecret: "pull"}
	if err := k.create(context.Background(), id, "private-worker-token", "falkon", "[]"); err != nil {
		t.Fatal(err)
	}
	address, err := k.waitRunning(context.Background(), id)
	if err != nil || address != "http://10.1.2.3:8081" {
		t.Fatal(address, err)
	}
	if alive, err := k.alive(context.Background(), id); !alive || err != nil {
		t.Fatal(alive, err)
	}
	mu.Lock()
	expected = "token-two"
	mu.Unlock()
	if err := os.WriteFile(tokenFile, []byte("token-two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := k.reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("reset did not delete owned pod")
	}
	if err := k.stop(context.Background(), id); err != nil {
		t.Fatal("idempotent stop", err)
	}
}
func TestKubernetesRejectsForeignPod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte("token"), 0600)
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted = true
		}
		_, _ = w.Write([]byte(`{"metadata":{"uid":"foreign","labels":{"mvp1.runner":"other"}},"status":{"phase":"Running"}}`))
	}))
	defer server.Close()
	k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: path}
	if err := k.stop(context.Background(), strings.Repeat("b", 64)); err == nil {
		t.Fatal("foreign pod accepted")
	}
	if deleted {
		t.Fatal("deleted foreign pod")
	}
}

func TestKubernetesDeletionWaitsAndRejectsReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("token"), 0600); err != nil {
				t.Fatal(err)
			}
			checks := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					w.WriteHeader(200)
					return
				}
				checks++
				if replace {
					_, _ = w.Write([]byte(`{"metadata":{"uid":"new-pod"}}`))
					return
				}
				if checks == 1 {
					_, _ = w.Write([]byte(`{"metadata":{"uid":"old-pod","deletionTimestamp":"2026-10-07T00:00:00Z"}}`))
					return
				}
				w.WriteHeader(404)
			}))
			defer server.Close()
			k := &kubernetes{client: server.Client(), base: server.URL, namespace: "test", tokenFile: token}
			var p kubePod
			p.Metadata.Name = "test-pod"
			p.Metadata.UID = "old-pod"
			p.Metadata.Labels = map[string]string{"mvp1.runner": "test", "app.kubernetes.io/component": "session"}
			err := k.remove(context.Background(), p)
			if replace && err == nil {
				t.Fatal("replacement ignored")
			}
			if !replace && (err != nil || checks != 2) {
				t.Fatal("did not wait for deletion", checks, err)
			}
		})
	}
}
