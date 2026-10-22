package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mvp1/internal/turn"
	"mvp1/internal/wire"
)

func randomToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}
func (r *Runner) splitNetwork(id string) string { return r.config.Namespace + "-site-" + id[:32] }
func dockerIP(ctx context.Context, name, network string) (string, error) {
	out, err := docker(ctx, "inspect", name, "--format", "{{json .NetworkSettings.Networks}}")
	if err != nil {
		return "", err
	}
	var networks map[string]struct{ IPAddress string }
	if err = json.Unmarshal(out, &networks); err != nil {
		return "", err
	}
	ip := net.ParseIP(networks[network].IPAddress)
	if ip == nil {
		return "", errors.New("container has no IP")
	}
	return ip.String(), nil
}
func runContainer(ctx context.Context, args, env []string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("container start failed: %w: %.500s", err, out)
	}
	return nil
}
func (r *Runner) startSplitDocker(ctx context.Context, v wire.Start, w worker, publicIP string, internal bool) (worker, error) {
	network := r.splitNetwork(v.ID)
	if _, err := docker(ctx, "network", "create", "--internal", "--driver", "bridge", "--opt", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "--label", "mvp1.namespace="+r.config.Namespace, network); err != nil {
		return w, err
	}
	policy, _ := json.Marshal(v.Access)
	agentToken, err := randomToken()
	if err != nil {
		return w, err
	}
	subnet, err := docker(ctx, "network", "inspect", network, "--format", "{{range .IPAM.Config}}{{.Subnet}}{{end}}")
	if err != nil {
		return w, err
	}
	// No forwarding between the isolated application network and the media network.
	port := strconv.Itoa(r.config.UDPBase + w.slot)
	credentials, _ := json.Marshal(turn.Credentials(r.config.TURNURL, r.config.TURNSecret, v.ID, time.Now()))
	args := []string{"run", "--detach", "--rm", "--init", "--name", w.name, "--label", "mvp1.namespace=" + r.config.Namespace, "--network", r.config.Network, "--sysctl", "net.ipv4.ip_forward=0", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=64", "--memory=256m", "--cpus=1", "-p", "127.0.0.1::8081/tcp", "-p", port + ":" + port + "/udp", "-e", "MVP_MODE=relay", "-e", "MVP_APP=" + v.Template, "-e", "MVP_UDP_PORT=" + port, "-e", "MVP_PUBLIC_IP=" + publicIP, "-e", "MVP_WORKER_TOKEN", "-e", "MVP_ICE_SERVERS", "-e", "MVP_ACCESS", "-e", "MVP_DENIED_CIDRS", "-e", "MVP_APP_NETWORK", r.config.Image}
	env := []string{"MVP_APP_NETWORK=" + strings.TrimSpace(string(subnet)), "MVP_WORKER_TOKEN=" + w.token, "MVP_ICE_SERVERS=" + string(credentials), "MVP_ACCESS=" + string(policy), "MVP_DENIED_CIDRS=" + strings.TrimSpace(string(subnet)) + "," + r.config.DeniedCIDRs}
	if err = runContainer(ctx, args, env); err != nil {
		return w, err
	}
	if _, err = docker(ctx, "network", "connect", network, w.name); err != nil {
		return w, err
	}
	workerIP, err := dockerIP(ctx, w.name, network)
	if err != nil {
		return w, err
	}
	if internal {
		ip, e := dockerIP(ctx, w.name, r.config.Network)
		if e != nil {
			return w, e
		}
		w.address = "http://" + net.JoinHostPort(ip, "8081")
	} else {
		out, e := docker(ctx, "port", w.name, "8081/tcp")
		if e != nil {
			return w, e
		}
		w.address = "http://" + strings.TrimSpace(string(out))
		if !strings.HasPrefix(w.address, "http://127.0.0.1:") {
			return w, errors.New("non-loopback control port")
		}
	}
	args = []string{"run", "--detach", "--rm", "--init", "--name", w.name + "-app", "--label", "mvp1.namespace=" + r.config.Namespace, "--network", network, "--dns", "127.0.0.1", "--read-only", "--cap-drop=ALL", "--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=KILL", "--security-opt=no-new-privileges", "--pids-limit=512", "--memory=1g", "--cpus=" + strconv.FormatFloat(r.config.BrowserCPUs, 'f', -1, 64), "--shm-size=256m", "--tmpfs", "/tmp:rw,nosuid,nodev,size=256m", "--tmpfs", "/home/app:rw,nosuid,nodev,size=256m,uid=1000,gid=1000", "-e", "MVP_MODE=app", "-e", "MVP_APP=" + v.Template, "-e", "MVP_UDP_PORT=0", "-e", "MVP_AGENT_TOKEN", "-e", "MVP_START_URL", "-e", "MVP_PROXY=http://" + net.JoinHostPort(workerIP, "8082"), "-e", "MVP_PRINT_TARGET=http://" + net.JoinHostPort(workerIP, "8631"), r.config.Image}
	if err = runContainer(ctx, args, []string{"MVP_AGENT_TOKEN=" + agentToken, "MVP_START_URL=" + v.StartURL}); err != nil {
		return w, err
	}
	appIP, err := dockerIP(ctx, w.name+"-app", network)
	if err != nil {
		return w, err
	}
	return w, r.attach(ctx, w, "http://"+net.JoinHostPort(appIP, "8090"), agentToken)
}
func (r *Runner) attach(ctx context.Context, w worker, address, token string) error {
	for {
		err := r.workerCall(ctx, w, "/attach", map[string]string{"Address": address, "Token": token}, nil)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}
func (r *Runner) removeSplitNetwork(ctx context.Context, id string) error {
	name := r.splitNetwork(id)
	out, err := docker(ctx, "network", "ls", "-q", "--filter", "label=mvp1.namespace="+r.config.Namespace, "--filter", "name=^"+name+"$")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "" {
		_, err = docker(ctx, "network", "rm", name)
	}
	return err
}
