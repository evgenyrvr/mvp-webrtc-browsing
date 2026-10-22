package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mvp1/internal/wire"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount/"

type kubernetes struct {
	client                                        *http.Client
	base, namespace, tokenFile, image, pullSecret string
}
type kubePod struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
		PodIP string `json:"podIP"`
	} `json:"status"`
}

func newKubernetes(cfg Config) (*kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if host == "" {
		return nil, errors.New("Kubernetes backend requires in-cluster service account")
	}
	if port == "" {
		port = "443"
	}
	ca, err := os.ReadFile(serviceAccountDir + "ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid Kubernetes CA")
	}
	return &kubernetes{client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, base: "https://" + net.JoinHostPort(host, port), namespace: cfg.Namespace, tokenFile: serviceAccountDir + "token", image: cfg.Image, pullSecret: cfg.ImagePullSecret}, nil
}
func (k *kubernetes) path() string {
	return "/api/v1/namespaces/" + url.PathEscape(k.namespace) + "/pods"
}
func (k *kubernetes) selector() string {
	return "mvp1.runner=" + k.namespace
}
func podName(id string) string { return "mvp1-" + id[:56] }

// Read the projected token on every call, allowing Kubernetes token rotation.
func (k *kubernetes) call(ctx context.Context, method, path string, in, out any) (int, error) {
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return 0, err
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return 404, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("Kubernetes API %s: HTTP %d", method, resp.StatusCode)
	}
	if out != nil {
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
	}
	return resp.StatusCode, err
}
func (k *kubernetes) list(ctx context.Context) ([]kubePod, error) {
	var list struct {
		Items []kubePod `json:"items"`
	}
	status, err := k.call(ctx, "GET", k.path()+"?labelSelector="+url.QueryEscape(k.selector()), nil, &list)
	if err == nil && status == 404 {
		err = errors.New("Kubernetes namespace not found")
	}
	return list.Items, err
}
func (k *kubernetes) owned(p kubePod) bool {
	return p.Metadata.Labels["mvp1.runner"] == k.namespace && (p.Metadata.Labels["app.kubernetes.io/component"] == "session" || p.Metadata.Labels["app.kubernetes.io/component"] == "isolated-app")
}
func (k *kubernetes) get(ctx context.Context, id string) (kubePod, bool, error) {
	var pod kubePod
	status, err := k.call(ctx, "GET", k.path()+"/"+podName(id), nil, &pod)
	if err != nil || status == 404 {
		return pod, false, err
	}
	if !k.owned(pod) {
		return pod, false, errors.New("pod belongs to another owner")
	}
	return pod, true, nil
}
func (k *kubernetes) pod(id, token, template, ice string) map[string]any {
	env := []map[string]any{{"name": "MVP_WORKER_TOKEN", "value": token}, {"name": "MVP_APP", "value": template}, {"name": "MVP_UDP_PORT", "value": "50000"}, {"name": "MVP_ICE_SERVERS", "value": ice}}
	volume := func(name, path, size string) (map[string]any, map[string]any) {
		return map[string]any{"name": name, "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": size}}, map[string]any{"name": name, "mountPath": path}
	}
	var volumes, mounts []map[string]any
	for _, v := range [][3]string{{"tmp", "/tmp", "256Mi"}, {"home", "/home/app", "256Mi"}, {"shm", "/dev/shm", "256Mi"}} {
		a, b := volume(v[0], v[1], v[2])
		volumes = append(volumes, a)
		mounts = append(mounts, b)
	}
	spec := map[string]any{
		"activeDeadlineSeconds": int64(wire.MaxSessionLifetime.Seconds()),
		"restartPolicy":         "Never", "automountServiceAccountToken": false, "serviceAccountName": "mvp1-session", "terminationGracePeriodSeconds": 2,
		"nodeSelector":    map[string]string{"kubernetes.io/os": "linux"},
		"securityContext": map[string]any{"fsGroup": 1000, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"volumes":         volumes,
		"containers": []map[string]any{{"name": "worker", "image": k.image, "imagePullPolicy": "IfNotPresent", "env": env, "volumeMounts": mounts,
			"securityContext": map[string]any{"runAsUser": 0, "runAsGroup": 0, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}, "add": []string{"SETUID", "SETGID", "KILL"}}},
			"resources":       map[string]any{"requests": map[string]string{"cpu": "250m", "memory": "256Mi", "ephemeral-storage": "64Mi"}, "limits": map[string]string{"cpu": "1500m", "memory": "1Gi", "ephemeral-storage": "256Mi"}},
			"ports":           []map[string]any{{"name": "control", "containerPort": 8081}, {"name": "media", "containerPort": 50000, "protocol": "UDP"}},
			"readinessProbe":  map[string]any{"tcpSocket": map[string]any{"port": 8081}, "periodSeconds": 1, "failureThreshold": 3},
		}},
	}
	if k.pullSecret != "" {
		spec["imagePullSecrets"] = []map[string]string{{"name": k.pullSecret}}
	}
	return map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": podName(id), "namespace": k.namespace, "labels": map[string]string{"mvp1.runner": k.namespace, "app.kubernetes.io/component": "session"}, "annotations": map[string]string{"mvp1.session-id": id}}, "spec": spec}
}
func (k *kubernetes) create(ctx context.Context, id, token, template, ice string) error {
	status, err := k.call(ctx, "POST", k.path(), k.pod(id, token, template, ice), nil)
	if err != nil {
		return err
	}
	if status == 404 {
		return errors.New("Kubernetes namespace not found")
	}
	return nil
}
func (k *kubernetes) waitRunning(ctx context.Context, id string) (string, error) {
	ip, err := k.waitNamed(ctx, podName(id))
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort(ip, "8081"), nil
}
func (k *kubernetes) waitNamed(ctx context.Context, name string) (string, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, exists, err := k.getNamed(ctx, name)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", errors.New("session pod disappeared")
		}
		if pod.Status.Phase == "Failed" || pod.Status.Phase == "Succeeded" || pod.Metadata.DeletionTimestamp != nil {
			return "", errors.New("session pod terminated during startup")
		}
		if pod.Status.Phase == "Running" && net.ParseIP(pod.Status.PodIP) != nil {
			if net.ParseIP(pod.Status.PodIP).To4() == nil {
				return "", errors.New("MVP requires IPv4 Pod addresses")
			}
			return pod.Status.PodIP, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
func (k *kubernetes) remove(ctx context.Context, p kubePod) error {
	if !k.owned(p) || p.Metadata.UID == "" {
		return errors.New("cannot delete unowned pod")
	}
	path := k.path() + "/" + url.PathEscape(p.Metadata.Name)
	_, err := k.call(ctx, "DELETE", path, map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": 2, "preconditions": map[string]string{"uid": p.Metadata.UID}}, nil)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		var current kubePod
		status, err := k.call(ctx, "GET", path, nil, &current)
		if err != nil {
			return err
		}
		if status == 404 {
			return nil
		}
		if current.Metadata.UID != p.Metadata.UID {
			return errors.New("pod replaced during deletion")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (k *kubernetes) stop(ctx context.Context, id string) error {
	if err := k.stopSplit(ctx, id); err != nil {
		return err
	}
	p, exists, err := k.get(ctx, id)
	if err != nil {
		return err
	}
	if exists {
		if err = k.remove(ctx, p); err != nil {
			return err
		}
	}
	return k.removePolicies(ctx, id)
}
func (k *kubernetes) reset(ctx context.Context) error {
	pods, err := k.list(ctx)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if err := k.remove(ctx, p); err != nil {
			return err
		}
	}
	return k.resetPolicies(ctx)
}
func (k *kubernetes) resetPolicies(ctx context.Context) error {
	var list struct {
		Items []kubePod `json:"items"`
	}
	if _, err := k.call(ctx, "GET", k.policyPath()+"?labelSelector="+url.QueryEscape("mvp1.runner="+k.namespace), nil, &list); err != nil {
		return err
	}
	for _, p := range list.Items {
		if p.Metadata.Labels["mvp1.runner"] != k.namespace || p.Metadata.UID == "" {
			return errors.New("unowned policy")
		}
		if _, err := k.call(ctx, "DELETE", k.policyPath()+"/"+url.PathEscape(p.Metadata.Name), map[string]any{"preconditions": map[string]string{"uid": p.Metadata.UID}}, nil); err != nil {
			return err
		}
	}
	return nil
}
func (k *kubernetes) alive(ctx context.Context, id string) (bool, error) {
	p, exists, err := k.get(ctx, id)
	alive := exists && p.Metadata.DeletionTimestamp == nil && (p.Status.Phase == "Pending" || p.Status.Phase == "Running")
	if err == nil && alive && p.Metadata.Labels["mvp1.part"] == "relay" {
		app, exists, e := k.getNamed(ctx, splitAppName(id))
		return exists && app.Metadata.DeletionTimestamp == nil && (app.Status.Phase == "Pending" || app.Status.Phase == "Running"), e
	}
	return alive, err
}
