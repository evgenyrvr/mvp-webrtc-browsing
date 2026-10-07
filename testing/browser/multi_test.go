package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type multiClient struct {
	call func(string, any) json.RawMessage
	eval func(string) json.RawMessage
}
type appTab struct {
	client     *multiClient
	handle, id string
	frames     float64
}

// Reuses the disposable gateway/runner harness, with two independent cookie jars.
func testTwoUsers(t *testing.T, ctx context.Context, dir, origin string, start func(string, []string, ...string), wait func(func() bool), port func() string) {
	t.Helper()
	browserApp := os.Getenv("MVP_BROWSER_APP")
	if browserApp == "" {
		browserApp = "firefox"
	}
	if browserApp != "firefox" && browserApp != "falkon" {
		t.Fatal("invalid browser application")
	}
	clients := make([]*multiClient, 2)
	decode := func(raw json.RawMessage) any {
		t.Helper()
		var v struct {
			Result struct {
				Value any `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if m, ok := v.Result.Value.(map[string]any); ok && m["error"] != nil {
			t.Fatalf("browser JS error: %v", m["error"])
		}
		return v.Result.Value
	}
	for i := range clients {
		profile := filepath.Join(dir, fmt.Sprintf("user-%d", i))
		if err := os.Mkdir(profile, 0700); err != nil {
			t.Fatal(err)
		}
		call := firefoxDriver(t, ctx, profile, port(), start, wait)
		c := &multiClient{call: call}
		c.eval = func(s string) json.RawMessage { return call("Runtime.evaluate", map[string]any{"expression": s}) }
		clients[i] = c
		call("Page.navigate", map[string]any{"url": origin})
		wait(func() bool { return decode(c.eval("document.readyState")) == "complete" })
	}
	request := func(c *multiClient, path, method string, body any) map[string]any {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		script := fmt.Sprintf(`fetch(%q,{method:%q,headers:{'Content-Type':'application/json','X-CSRF-Token':window.multiCSRF||''},body:%s}).then(async r=>({status:r.status,data:await r.json()}))`, path, method, "undefined")
		if body != nil {
			script = fmt.Sprintf(`fetch(%q,{method:%q,headers:{'Content-Type':'application/json','X-CSRF-Token':window.multiCSRF||''},body:JSON.stringify(%s)}).then(async r=>({status:r.status,data:await r.json()}))`, path, method, data)
		}
		v, ok := decode(c.eval(script)).(map[string]any)
		if !ok {
			t.Fatal("invalid response")
		}
		return v
	}
	success := func(v map[string]any) any {
		t.Helper()
		status := v["status"].(float64)
		if status < 200 || status >= 300 {
			t.Fatalf("API failed: %v", v)
		}
		return v["data"]
	}
	login := func(c *multiClient, name, password string) {
		t.Helper()
		v := success(request(c, "/api/v1/login", "POST", map[string]any{"login": name, "password": password})).(map[string]any)
		c.eval(fmt.Sprintf("window.multiCSRF=%q", v["csrf_token"]))
	}
	login(clients[0], "browser-test", "temporary-browser-test")
	for _, app := range []string{browserApp, "terminal"} {
		success(request(clients[0], "/api/v1/admin/grants", "PUT", map[string]any{"role_id": 2, "application_id": app, "can_launch": true, "can_control": true, "can_print": app == browserApp}))
	}
	for _, name := range []string{"alice", "bob"} {
		success(request(clients[0], "/api/v1/admin/users", "PUT", map[string]any{"login": name, "password": "temporary-multi-test", "enabled": true, "role_ids": []int{2}}))
	}
	success(request(clients[0], "/api/v1/logout", "POST", map[string]any{}))
	var tabs []*appTab
	for user, c := range clients {
		login(c, []string{"alice", "bob"}[user], "temporary-multi-test")
		if status := request(c, "/api/v1/admin", "GET", nil)["status"]; status != float64(403) {
			t.Fatalf("ordinary user admin access: %v", status)
		}
		for _, app := range []string{browserApp, "terminal"} {
			v := success(request(c, "/api/v1/sessions", "POST", map[string]any{"application_id": app})).(map[string]any)
			id, ok := v["id"].(string)
			if !ok {
				t.Fatal(v)
			}
			start("docker", nil, "logs", "--follow", "mvp1-browser-"+id)
			var window struct {
				Handle string `json:"handle"`
			}
			if err := json.Unmarshal(c.call("WebDriver:NewWindow", map[string]any{"type": "tab"}), &window); err != nil || window.Handle == "" {
				t.Fatalf("new tab: %v", err)
			}
			c.call("WebDriver:SwitchToWindow", map[string]any{"handle": window.Handle})
			c.call("Page.navigate", map[string]any{"url": origin})
			wait(func() bool { return decode(c.eval("document.readyState")) == "complete" })
			c.eval(`window.multiPeers=[];window.RTCPeerConnection=class extends RTCPeerConnection{constructor(...args){super(...args);window.multiPeers.push(this)}}`)
			c.eval(fmt.Sprintf(`history.pushState({},'', '/sessions/'+%q);refreshIdentity()`, id))
			tab := &appTab{client: c, handle: window.Handle, id: id}
			tabs = append(tabs, tab)
			wait(func() bool {
				v := decode(c.eval(`(async()=>{const p=window.multiPeers.at(-1);if(!p||p.connectionState!=='connected')return false;let n=0;(await p.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='video')n+=s.framesDecoded||0});return n>10&&document.getElementById('remote-video').videoWidth===1280})()`))
				return v == true
			})
			// The new tab gets its own CSRF variable but shares this user's auth cookie.
			c.eval(`fetch('/api/v1/me').then(r=>r.json()).then(v=>window.multiCSRF=v.csrf_token)`)
		}
		if status := request(c, "/api/v1/sessions", "POST", map[string]any{"application_id": "terminal"})["status"]; status != float64(409) {
			t.Fatalf("third application not rejected: %v", status)
		}
	}
	for i, tab := range tabs {
		tab.client.call("WebDriver:SwitchToWindow", map[string]any{"handle": tab.handle})
		other := tabs[(i+2)%4].id
		if status := request(tab.client, "/api/v1/sessions/"+other, "GET", nil)["status"]; status != float64(404) {
			t.Fatalf("foreign session exposed: %v", status)
		}
	}
	observe := func(active []*appTab, rounds int) {
		t.Helper()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for n := 0; n < rounds; n++ {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-ticker.C:
			}
			for _, tab := range active {
				tab.client.call("WebDriver:SwitchToWindow", map[string]any{"handle": tab.handle})
				v := decode(tab.client.eval(`(async()=>{const p=window.multiPeers.at(-1);let frames=0;(await p.getStats()).forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='video')frames+=s.framesDecoded||0});return {frames,connected:p.connectionState==='connected',peers:window.multiPeers.length}})()`)).(map[string]any)
				frames := v["frames"].(float64)
				if v["connected"] != true || v["peers"] != float64(1) || frames <= tab.frames {
					t.Fatalf("tab stopped or reconnected: %v", v)
				}
				tab.frames = frames
			}
		}
	}
	observe(tabs, 5)
	t.Log("two independent users, four tabs: decoded video grows without reconnects")
	clients[0].call("WebDriver:SwitchToWindow", map[string]any{"handle": tabs[0].handle})
	success(request(clients[0], "/api/v1/logout", "POST", map[string]any{}))
	wait(func() bool {
		for _, tab := range tabs[:2] {
			if exec.CommandContext(ctx, "docker", "inspect", "mvp1-browser-"+tab.id).Run() == nil {
				return false
			}
		}
		return true
	})
	observe(tabs[2:], 3)
	t.Log("logout removed both first user's containers; second user's two tabs kept decoding")
}
