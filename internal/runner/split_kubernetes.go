package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"

	"mvp1/internal/wire"
)

func splitAppName(id string) string { return "app-" + id[:56] }
func (k *kubernetes) policyPath() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(k.namespace) + "/networkpolicies"
}
func splitLabels(id, role string) map[string]string {
	return map[string]string{"mvp1.session": id[:56], "mvp1.part": role}
}
func selector(labels map[string]string) map[string]any { return map[string]any{"matchLabels": labels} }
func (k *kubernetes) splitPolicy(id, role string) map[string]any {
	peerRole, peerPort := "app", 8090
	if role == "app" {
		peerRole, peerPort = "relay", 8082
	}
	peer := map[string]any{"podSelector": selector(splitLabels(id, peerRole))}
	ports := []map[string]any{{"protocol": "TCP", "port": peerPort}}
	if role == "app" {
		ports = append(ports, map[string]any{"protocol": "TCP", "port": 8631})
	}
	egress := []map[string]any{{"to": []map[string]any{peer}, "ports": ports}}
	ingressPorts := []map[string]any{{"protocol": "TCP", "port": 8090}}
	if role == "relay" {
		ingressPorts = []map[string]any{{"protocol": "TCP", "port": 8082}, {"protocol": "TCP", "port": 8631}}
		// Only the trusted proxy has destination egress; app Pods never match this selector.
		egress = append(egress, map[string]any{"to": []map[string]any{{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}, "ports": []map[string]any{{"protocol": "TCP"}}})
	}
	return map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"name": podName(id) + "-" + role, "namespace": k.namespace, "labels": map[string]string{"mvp1.runner": k.namespace}}, "spec": map[string]any{"podSelector": selector(splitLabels(id, role)), "policyTypes": []string{"Ingress", "Egress"}, "ingress": []map[string]any{{"from": []map[string]any{peer}, "ports": ingressPorts}}, "egress": egress}}
}
func (r *Runner) startSplitKube(ctx context.Context, v wire.Start, w worker, ice string) (worker, error) {
	k := r.kube
	// Policies must exist before either Pod is started.
	for _, role := range []string{"app", "relay"} {
		if err := k.createResource(ctx, k.policyPath(), k.splitPolicy(v.ID, role)); err != nil {
			return w, err
		}
	}
	policy, _ := json.Marshal(v.Access)
	pod := k.pod(v.ID, w.token, v.Template, ice)
	metadata := pod["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]string)
	for key, value := range splitLabels(v.ID, "relay") {
		labels[key] = value
	}
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]map[string]any)[0]
	env := container["env"].([]map[string]any)
	container["env"] = append(env, map[string]any{"name": "MVP_MODE", "value": "relay"}, map[string]any{"name": "MVP_ACCESS", "value": string(policy)}, map[string]any{"name": "MVP_DENIED_CIDRS", "value": r.config.DeniedCIDRs})
	container["securityContext"].(map[string]any)["capabilities"] = map[string]any{"drop": []string{"ALL"}}
	container["resources"] = map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "64Mi"}, "limits": map[string]string{"cpu": "1000m", "memory": "256Mi"}}
	if err := k.createResource(ctx, k.path(), pod); err != nil {
		return w, err
	}
	address, err := k.waitRunning(ctx, v.ID)
	if err != nil {
		return w, err
	}
	w.address = address
	host, _, err := net.SplitHostPort(strings.TrimPrefix(address, "http://"))
	if err != nil {
		return w, err
	}
	agentToken, err := randomToken()
	if err != nil {
		return w, err
	}
	app := k.pod(v.ID, "", v.Template, "[]")
	am := app["metadata"].(map[string]any)
	am["name"] = splitAppName(v.ID)
	al := am["labels"].(map[string]string)
	al["app.kubernetes.io/component"] = "isolated-app"
	for key, value := range splitLabels(v.ID, "app") {
		al[key] = value
	}
	as := app["spec"].(map[string]any)
	as["dnsPolicy"] = "None"
	as["dnsConfig"] = map[string]any{"nameservers": []string{"127.0.0.1"}}
	ac := as["containers"].([]map[string]any)[0]
	ac["name"] = "app"
	ac["env"] = []map[string]any{{"name": "MVP_MODE", "value": "app"}, {"name": "MVP_APP", "value": v.Template}, {"name": "MVP_UDP_PORT", "value": "0"}, {"name": "MVP_AGENT_TOKEN", "value": agentToken}, {"name": "MVP_START_URL", "value": v.StartURL}, {"name": "MVP_PROXY", "value": "http://" + net.JoinHostPort(host, "8082")}, {"name": "MVP_PRINT_TARGET", "value": "http://" + net.JoinHostPort(host, "8631")}}
	ac["ports"] = []map[string]any{{"name": "agent", "containerPort": 8090}}
	ac["readinessProbe"] = map[string]any{"tcpSocket": map[string]any{"port": 8090}, "periodSeconds": 1}
	if err = k.createResource(ctx, k.path(), app); err != nil {
		return w, err
	}
	appAddress, err := k.waitNamed(ctx, splitAppName(v.ID))
	if err != nil {
		return w, err
	}
	return w, r.attach(ctx, w, "http://"+net.JoinHostPort(appAddress, "8090"), agentToken)
}
func (k *kubernetes) getNamed(ctx context.Context, name string) (kubePod, bool, error) {
	var p kubePod
	status, err := k.call(ctx, "GET", k.path()+"/"+name, nil, &p)
	if err != nil || status == 404 {
		return p, false, err
	}
	if !k.owned(p) {
		return p, false, errors.New("unowned pod")
	}
	return p, true, nil
}
func (k *kubernetes) stopSplit(ctx context.Context, id string) error {
	p, exists, err := k.getNamed(ctx, splitAppName(id))
	if err != nil {
		return err
	}
	if exists {
		if err = k.remove(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func (k *kubernetes) removePolicies(ctx context.Context, id string) error {
	for _, role := range []string{"app", "relay"} {
		path := k.policyPath() + "/" + podName(id) + "-" + role
		var p kubePod
		status, err := k.call(ctx, "GET", path, nil, &p)
		if err != nil {
			return err
		}
		if status == 404 {
			continue
		}
		if p.Metadata.Labels["mvp1.runner"] != k.namespace || p.Metadata.UID == "" {
			return errors.New("unowned network policy")
		}
		if _, err = k.call(ctx, "DELETE", path, map[string]any{"preconditions": map[string]string{"uid": p.Metadata.UID}}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (k *kubernetes) createResource(ctx context.Context, path string, v any) error {
	status, err := k.call(ctx, "POST", path, v, nil)
	if err != nil {
		return err
	}
	if status != 201 {
		return errors.New("Kubernetes resource was not created")
	}
	return nil
}
