package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Minimal adapter for the Mozilla Marionette protocol; uses a disposable profile.
func firefoxDriver(t *testing.T, ctx context.Context, dir, port string, start func(string, []string, ...string), wait func(func() bool)) func(string, any) json.RawMessage {
	t.Helper()
	profile := filepath.Join(dir, "firefox-profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	prefs := fmt.Sprintf("user_pref(\"marionette.port\", %s);\nuser_pref(\"browser.shell.checkDefaultBrowser\", false);\n", port)
	if err := os.WriteFile(filepath.Join(profile, "user.js"), []byte(prefs), 0600); err != nil {
		t.Fatal(err)
	}
	start("firefox", nil, "--headless", "--no-remote", "--profile", profile, "--marionette", "about:blank")
	var conn net.Conn
	wait(func() bool {
		var err error
		conn, err = net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		return err == nil
	})
	t.Cleanup(func() { conn.Close() })
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	reader := bufio.NewReader(conn)
	receive := func() json.RawMessage {
		t.Helper()
		prefix, err := reader.ReadString(':')
		if err != nil {
			t.Fatal(err)
		}
		size, err := strconv.Atoi(strings.TrimSuffix(prefix, ":"))
		if err != nil || size > 4<<20 {
			t.Fatal("invalid Marionette frame")
		}
		buf := make([]byte, size)
		if _, err = io.ReadFull(reader, buf); err != nil {
			t.Fatal(err)
		}
		return buf
	}
	_ = receive()
	id := 0
	request := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		body, _ := json.Marshal([]any{0, id, method, params})
		if _, err := fmt.Fprintf(conn, "%d:%s", len(body), body); err != nil {
			t.Fatal(err)
		}
		var response []json.RawMessage
		if err := json.Unmarshal(receive(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response) != 4 {
			t.Fatal("invalid Marionette reply")
		}
		if string(response[2]) != "null" {
			t.Fatalf("%s: %s", method, response[2])
		}
		return response[3]
	}
	request("WebDriver:NewSession", map[string]any{"acceptInsecureCerts": true})
	return func(method string, params any) json.RawMessage {
		t.Helper()
		if strings.HasPrefix(method, "WebDriver:") {
			return request(method, params)
		}
		p := params.(map[string]any)
		switch method {
		case "Runtime.enable", "Page.enable", "Network.enable":
			return json.RawMessage(`{}`)
		case "Page.navigate":
			return request("WebDriver:Navigate", map[string]any{"url": p["url"]})
		case "Runtime.evaluate":
			script := "const done = arguments[arguments.length-1]; Promise.resolve(eval("
			expr, _ := json.Marshal(p["expression"])
			script += string(expr) + ")).then(done, e => done({error:String(e)}));"
			result := request("WebDriver:ExecuteAsyncScript", map[string]any{"script": script, "args": []any{}, "newSandbox": false, "sandbox": nil, "scriptTimeout": 30000})
			var v struct {
				Value any `json:"value"`
			}
			if err := json.Unmarshal(result, &v); err != nil {
				t.Fatal(err)
			}
			out, _ := json.Marshal(map[string]any{"result": map[string]any{"value": v.Value}})
			return out
		default:
			t.Fatalf("unsupported method %s", method)
			return nil
		}
	}
}
