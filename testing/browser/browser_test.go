package browser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	ipp "github.com/OpenPrinting/goipp"
	_ "modernc.org/sqlite"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestBrowserVideo(t *testing.T) {
	if os.Getenv("MVP_BROWSER_TEST") != "1" {
		t.Skip("set MVP_BROWSER_TEST=1; requires built binaries, Docker image and Chromium")
	}
	root, _ := filepath.Abs("../..")
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp(filepath.Join(root, "testing/data"), "browser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	start := func(name string, env []string, args ...string) {
		t.Helper()
		log, err := os.CreateTemp(dir, filepath.Base(name)+"-*.log")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
			log.Close()
			if t.Failed() && (filepath.Base(name) == "gateway" || filepath.Base(name) == "runner" || filepath.Base(name) == "docker") {
				content, _ := os.ReadFile(log.Name())
				t.Logf("%s log: %s", filepath.Base(name), content)
			}
		})
	}
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return strings.Split(l.Addr().String(), ":")[1]
	}
	gatewayPort, debugPort := port(), port()
	origin := "http://127.0.0.1:" + gatewayPort
	network, publicIP := "mvp1-integration", "127.0.0.1"
	gatewayAddr := "127.0.0.1:" + gatewayPort
	var tlsArgs []string
	if ip := os.Getenv("MVP_BROWSER_LAN_IP"); ip != "" {
		if net.ParseIP(ip) == nil {
			t.Fatal("invalid LAN IP")
		}
		publicIP = ip
		network = "mvp1-browser-lan"
		gatewayAddr = "0.0.0.0:" + gatewayPort
		origin = "https://" + ip + ":" + gatewayPort
		cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		if out, err := exec.CommandContext(ctx, "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", key, "-out", cert, "-subj", "/CN="+ip, "-addext", "subjectAltName=IP:"+ip).CombinedOutput(); err != nil {
			t.Fatalf("cert: %v %s", err, out)
		}
		tlsArgs = []string{"-tls-cert", cert, "-tls-key", key}
	}
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", network).Run(); err != nil {
		args := []string{"network", "create"}
		if publicIP == "127.0.0.1" {
			args = append(args, "--internal")
		}
		args = append(args, network)
		if err := exec.CommandContext(ctx, "docker", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	workerImage := os.Getenv("MVP_WORKER_IMAGE")
	if workerImage == "" {
		workerImage = "mvp1-worker:pilot"
	}
	socket := filepath.Join(dir, "runner.sock")
	start(filepath.Join(root, "bin/runner"), nil, "-socket", socket, "-image", workerImage, "-namespace", "mvp1-browser", "-network", network, "-public-ip", publicIP, "-udp-base", "50200")
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !check() {
			if time.Now().After(deadline) {
				t.Fatal("condition did not become true within 30 seconds")
			}
			select {
			case <-ctx.Done():
				t.Fatal("timeout")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	wait(func() bool { _, err := os.Stat(socket); return err == nil })
	gatewayArgs := append([]string{"-db", filepath.Join(dir, "db"), "-addr", gatewayAddr, "-origin", origin, "-runner-socket", socket}, tlsArgs...)
	if os.Getenv("MVP_RELAY_ONLY") == "1" {
		gatewayArgs = append(gatewayArgs, "-relay-only")
	}
	start(filepath.Join(root, "bin/gateway"), []string{"MVP_ADMIN_LOGIN=browser-test", "MVP_ADMIN_PASSWORD=temporary-browser-test"}, gatewayArgs...)
	healthClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // Disposable test certificate only.
	wait(func() bool {
		resp, err := healthClient.Get(origin + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	if os.Getenv("MVP_BROWSER_MULTI") == "1" {
		testTwoUsers(t, ctx, dir, origin, start, wait, port)
		return
	}
	var call func(string, any) json.RawMessage
	if os.Getenv("MVP_BROWSER_KIND") == "firefox" {
		call = firefoxDriver(t, ctx, dir, debugPort, start, wait)
	} else {
		start("chromium", nil, "--headless", "--ignore-certificate-errors", "--no-first-run", "--disable-dev-shm-usage", "--user-data-dir="+filepath.Join(dir, "profile"), "--remote-debugging-port="+debugPort, "about:blank")
		var targets []struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			Type                 string `json:"type"`
		}
		wait(func() bool {
			resp, err := http.Get("http://127.0.0.1:" + debugPort + "/json/list")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			return json.NewDecoder(resp.Body).Decode(&targets) == nil && len(targets) > 0
		})
		endpoint := ""
		for _, v := range targets {
			if v.Type == "page" {
				endpoint = v.WebSocketDebuggerURL
				break
			}
		}
		conn, _, err := websocket.Dial(ctx, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseNow()
		conn.SetReadLimit(4 << 20)
		next := 0
		call = func(method string, params any) json.RawMessage {
			t.Helper()
			next++
			if err := wsjson.Write(ctx, conn, map[string]any{"id": next, "method": method, "params": params}); err != nil {
				t.Fatal(err)
			}
			for {
				var result struct {
					ID     int             `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  any             `json:"error"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := wsjson.Read(ctx, conn, &result); err != nil {
					t.Fatal(err)
				}
				if result.Method == "Runtime.exceptionThrown" || result.Method == "Network.loadingFailed" {
					t.Logf("JS exception: %s", result.Params)
				}
				if result.ID == next {
					if result.Error != nil {
						t.Fatal(result.Error)
					}
					return result.Result
				}
			}
		}

	}
	eval := func(expression string) json.RawMessage {
		t.Helper()
		return call("Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true})
	}
	call("Runtime.enable", map[string]any{})
	call("Network.enable", map[string]any{})
	call("Page.enable", map[string]any{})
	call("Page.navigate", map[string]any{"url": origin})
	wait(func() bool { return strings.Contains(string(eval("document.readyState")), "complete") })
	t.Logf("login: %s", eval(`fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({login:'browser-test',password:'temporary-browser-test'})}).then(r=>r.json()).then(v=>{window.testCSRF=v.csrf_token;return !!v.csrf_token})`))
	eval(`window.testPDFs=[];const makeURL=URL.createObjectURL.bind(URL);URL.createObjectURL=b=>{window.testPDFs.push(b);return makeURL(b)};window.testPeers=[];window.testEvents=[];new MutationObserver(()=>window.testEvents.push(['status',document.getElementById('session-state').textContent])).observe(document.getElementById('session-state'),{childList:true});window.RTCPeerConnection=class extends RTCPeerConnection {createDataChannel(...args){const dc=super.createDataChannel(...args);(this.testChannels ||= {})[args[0]]=dc;return dc;}constructor(...args){super(...args);window.testPeers.push(this);this.addEventListener('icecandidate',e=>window.testEvents.push(['candidate',e.candidate?.candidate]));for(const name of ['icegatheringstatechange','iceconnectionstatechange','connectionstatechange'])this.addEventListener(name,()=>window.testEvents.push([name,this.iceGatheringState,this.iceConnectionState,this.connectionState]));}};window.WebSocket=class extends WebSocket{constructor(...args){super(...args);this.addEventListener('message',e=>{try{const v=JSON.parse(e.data);window.testEvents.push(['ws',v.type,v.message||'',v.sdp?.sdp?.split('\r\n').filter(l=>l.startsWith('a=candidate:'))])}catch(_){}});this.addEventListener('close',e=>window.testEvents.push(['close',e.code]));}}`)
	// Docker interface changes invalidate an in-flight Chromium fetch on this host.
	// Use a separate authenticated HTTP client for provisioning only; WebRTC,
	// rendering, input, print reception and takeover still run in the browser.
	var provision func(string) string
	var finish func(string)
	if os.Getenv("MVP_BROWSER_KIND") != "firefox" {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Transport: healthClient.Transport, Timeout: 45 * time.Second}
		post := func(path, csrf string, body any) map[string]any {
			t.Helper()
			data, _ := json.Marshal(body)
			req, err := http.NewRequestWithContext(ctx, "POST", origin+path, bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", origin)
			if csrf != "" {
				req.Header.Set("X-CSRF-Token", csrf)
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var v map[string]any
			if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				t.Fatalf("provisioning failed: %d %v", res.StatusCode, v)
			}
			return v
		}
		auth := post("/api/v1/login", "", map[string]any{"login": "browser-test", "password": "temporary-browser-test"})
		csrf := auth["csrf_token"].(string)
		provision = func(app string) string {
			return post("/api/v1/sessions", csrf, map[string]any{"application_id": app})["id"].(string)
		}
		finish = func(id string) {
			t.Helper()
			req, err := http.NewRequestWithContext(ctx, "DELETE", origin+"/api/v1/sessions/"+id, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Origin", origin)
			req.Header.Set("X-CSRF-Token", csrf)
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("cleanup HTTP status: %d", res.StatusCode)
			}
		}

	}
	browserApp := os.Getenv("MVP_BROWSER_APP")
	if browserApp == "" {
		browserApp = "firefox"
	}
	if browserApp != "firefox" && browserApp != "falkon" {
		t.Fatal("invalid browser application")
	}
	for _, app := range []string{browserApp, "terminal"} {
		if provision != nil {
			eval(fmt.Sprintf("window.testID=%q", provision(app)))
		} else {
			t.Logf("launch %s: %s", app, eval(fmt.Sprintf(`fetch('/api/v1/sessions',{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':window.testCSRF},body:JSON.stringify({application_id:%q})}).then(r=>r.json()).then(v=>{window.testID=v.id;return v})`, app)))
		}
		idResult := eval(`window.testID`)
		var launched struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(idResult, &launched); err != nil {
			t.Fatal(err)
		}
		if len(launched.Result.Value) == 64 {
			start("docker", nil, "logs", "--follow", "mvp1-browser-"+launched.Result.Value)
		}
		eval(`history.pushState({},'', '/sessions/'+window.testID); refreshIdentity()`)
		result := eval(`new Promise(resolve=>{let n=0;const timer=setInterval(async()=>{const v=document.getElementById('remote-video');let frames=0;const pc=window.testPeers.at(-1);if(pc&&pc.connectionState!=='closed'){const stats=await pc.getStats();stats.forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='video')frames+=s.framesDecoded||0})}if(frames>10||++n>=40){clearInterval(timer);let brightPixels=0;if(v.videoWidth){const c=document.createElement('canvas');c.width=160;c.height=90;const x=c.getContext('2d');x.drawImage(v,0,0,160,90);const pixels=x.getImageData(0,0,160,90).data;for(let i=0;i<pixels.length;i+=4)if(pixels[i]+pixels[i+1]+pixels[i+2]>150)brightPixels++;}resolve({frames,brightPixels,width:v.videoWidth,height:v.videoHeight,state:document.getElementById('session-state').textContent,message:document.getElementById('message').textContent})}},500)})`)
		t.Logf("%s video: %s", app, result)
		var decoded struct {
			Result struct {
				Value struct {
					Frames       int `json:"frames"`
					BrightPixels int `json:"brightPixels"`
				} `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(result, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Result.Value.Frames <= 10 || decoded.Result.Value.BrightPixels < 10 {
			t.Logf("RTC diagnostics: %s", eval(`({events:window.testEvents,peers:window.testPeers.map(p=>({ice:p.iceConnectionState,gathering:p.iceGatheringState,state:p.connectionState}))})`))
			t.Fatalf("%s video did not decode", app)
		}
		holdSeconds := 12
		if value := os.Getenv("MVP_BROWSER_HOLD_SECONDS"); value != "" {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 2 || parsed > 60 {
				t.Fatal("hold seconds must be 2..60")
			}
			holdSeconds = parsed
		}
		eval(`window.holdPeer=window.testPeers.at(-1);window.holdPeers=window.testPeers.length;window.holdFrames=0`)
		for elapsed := 0; elapsed < holdSeconds; elapsed += 2 {
			stable := eval(`new Promise(resolve=>setTimeout(async()=>{let frames=0;const p=window.holdPeer;if(p.connectionState==='connected'){const stats=await p.getStats();stats.forEach(s=>{if(s.type==='inbound-rtp'&&s.kind==='video')frames+=s.framesDecoded||0})}const ok=p.connectionState==='connected'&&window.testPeers.length===window.holdPeers&&frames>window.holdFrames;window.holdFrames=frames;resolve({stable:ok,frames,state:p.connectionState,peers:window.testPeers.length})},2000))`)
			var result struct {
				Result struct {
					Value struct {
						Stable bool `json:"stable"`
					} `json:"value"`
				} `json:"result"`
			}
			if err := json.Unmarshal(stable, &result); err != nil {
				t.Fatal(err)
			}
			if !result.Result.Value.Stable {
				t.Logf("events: %s", eval(`window.testEvents`))
				t.Fatalf("%s connection interrupted after %ds: %s", app, elapsed+2, stable)
			}
		}
		t.Logf("%s: video continuous for %ds without reconnection", app, holdSeconds)
		if os.Getenv("MVP_RELAY_ONLY") == "1" {
			relay := eval(`window.testPeers.at(-1).getStats().then(stats=>{let pair;stats.forEach(s=>{if(s.type==='transport'&&s.selectedCandidatePairId)pair=stats.get(s.selectedCandidatePairId)});if(!pair)stats.forEach(s=>{if(s.type==='candidate-pair'&&s.nominated&&s.state==='succeeded')pair=s});const local=pair&&stats.get(pair.localCandidateId);return {relay:local?.candidateType==='relay',candidateType:local?.candidateType,protocol:local?.protocol}})`)
			var selected struct {
				Result struct {
					Value struct {
						Relay bool `json:"relay"`
					} `json:"value"`
				} `json:"result"`
			}
			if err := json.Unmarshal(relay, &selected); err != nil {
				t.Fatal(err)
			}
			if !selected.Result.Value.Relay {
				t.Fatalf("relay-only did not select relay: %s", relay)
			}
			t.Logf("%s selected ICE: %s", app, relay)
		}
		if app == "terminal" {
			eval(`(()=>{const stage=document.getElementById('remote-stage');stage.focus();const text='touch /tmp/mvp1-browser-proof';const codes=[...text].map(c=>/[a-z]/.test(c)?'Key'+c.toUpperCase():/[0-9]/.test(c)?'Digit'+c:({' ':'Space','/':'Slash','-':'Minus'})[c]);codes.push('Enter');for(const code of codes)for(const type of ['keydown','keyup'])stage.dispatchEvent(new KeyboardEvent(type,{code,bubbles:true,cancelable:true}));})()`)
			deadline := time.Now().Add(5 * time.Second)
			for {
				err := exec.CommandContext(ctx, "docker", "exec", "mvp1-browser-"+launched.Result.Value, "test", "-f", "/tmp/mvp1-browser-proof").Run()
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("browser keyboard did not reach terminal")
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Log("browser keyboard reached terminal over DataChannel")
		}
		if app == browserApp {
			if os.Getenv("MVP_PRINT_DIALOG_TEST") == "1" {
				eval(`(()=>{const stage=document.getElementById('remote-stage');stage.focus();for(const [type,code] of [['keydown','ControlLeft'],['keydown','KeyP'],['keyup','KeyP'],['keyup','ControlLeft']])stage.dispatchEvent(new KeyboardEvent(type,{code,bubbles:true,cancelable:true}));})()`)
				time.Sleep(4 * time.Second)
				shot, err := exec.CommandContext(ctx, "docker", "exec", "mvp1-browser-"+launched.Result.Value, "ffmpeg", "-loglevel", "error", "-f", "x11grab", "-video_size", "1280x720", "-i", ":99", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "-").Output()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile("/tmp/mvp1-print-dialog.png", shot, 0600); err != nil {
					t.Fatal(err)
				}

				if browserApp == "firefox" {
					eval(`(()=>{const stage=document.getElementById('remote-stage');for(const type of ['keydown','keyup'])stage.dispatchEvent(new KeyboardEvent(type,{code:'Escape',bubbles:true,cancelable:true}));})()`)

					eval(`(()=>{const dc=window.testPeers.at(-1).testChannels.control;for(const down of [true,false])dc.send(JSON.stringify({v:1,type:'button',button:0,down,x:1097/1280,y:685/720}));})()`)
					wait(func() bool {
						return strings.Contains(string(eval(`!!document.querySelector('#print-documents a')`)), "true")
					})
					result := eval(`window.testPDFs.at(-1).slice(0,5).text()`)
					if !strings.Contains(string(result), "%PDF-") {
						t.Fatalf("Firefox did not produce PDF: %s", result)
					}
					t.Log("Firefox print dialog submitted a real PDF and browser received it")
				} else {
					eval(`(()=>{const dc=window.testPeers.at(-1).testChannels.control;for(const down of [true,false])dc.send(JSON.stringify({v:1,type:'button',button:0,down,x:1143/1280,y:697/720}));})()`)
					time.Sleep(3 * time.Second)
					shot, err = exec.CommandContext(ctx, "docker", "exec", "mvp1-browser-"+launched.Result.Value, "ffmpeg", "-loglevel", "error", "-f", "x11grab", "-video_size", "1280x720", "-i", ":99", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "-").Output()
					if err == nil {
						_ = os.WriteFile("/tmp/mvp1-print-after.png", shot, 0600)
					}
					wait(func() bool {
						return strings.Contains(string(eval(`!!document.querySelector('#print-documents a')`)), "true")
					})
					result := eval(`window.testPDFs.at(-1).slice(0,5).text()`)
					if !strings.Contains(string(result), "%PDF-") {
						t.Fatalf("Falkon did not produce PDF: %s", result)
					}
					t.Log("Falkon print dialog submitted a real PDF and browser received it")
				}
			}
			document := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Count 0/Kids[]>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")
			request := ipp.NewRequest(ipp.DefaultVersion, ipp.OpPrintJob, 1)
			request.Operation = ipp.Attributes{ipp.MakeAttr("attributes-charset", ipp.TagCharset, ipp.String("utf-8")), ipp.MakeAttr("attributes-natural-language", ipp.TagLanguage, ipp.String("en")), ipp.MakeAttr("printer-uri", ipp.TagURI, ipp.String("ipp://127.0.0.1:8631/printers/local")), ipp.MakeAttr("document-format", ipp.TagMimeType, ipp.String("application/pdf"))}
			var body bytes.Buffer
			if err := request.Encode(&body); err != nil {
				t.Fatal(err)
			}
			body.Write(document)
			var httpRequest bytes.Buffer
			fmt.Fprintf(&httpRequest, "POST /printers/local HTTP/1.1\r\nHost: 127.0.0.1:8631\r\nContent-Type: application/ipp\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", body.Len())
			httpRequest.Write(body.Bytes())
			command := exec.CommandContext(ctx, "docker", "exec", "-i", "--user", "1000:1000", "mvp1-browser-"+launched.Result.Value, "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8631; cat >&3; cat <&3")
			command.Stdin = &httpRequest
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("IPP submission: %v %s", err, output)
			}
			wait(func() bool {
				return strings.Contains(string(eval(`!!document.querySelector('#print-documents a')`)), "true")
			})
			wait(func() bool {
				return strings.Contains(string(eval(fmt.Sprintf(`window.testPDFs.at(-1)?.size===%d`, len(document)))), "true")
			})
			received := eval(`window.testPDFs.at(-1).arrayBuffer().then(b=>crypto.subtle.digest('SHA-256',b)).then(b=>Array.from(new Uint8Array(b),v=>v.toString(16).padStart(2,'0')).join(''))`)
			if !strings.Contains(string(received), fmt.Sprintf("%x", sha256.Sum256(document))) {
				t.Fatalf("PDF checksum mismatch: %s", received)
			}
			t.Log("IPP PDF reached browser over print DataChannel with matching SHA-256")
			ui := eval(`(()=>{for(const code of ['ru','us']){document.getElementById('layout-'+code).click();if(document.getElementById('layout-'+code).getAttribute('aria-pressed')!=='true')return false;}const panel=document.getElementById('print-documents');for(const a of panel.querySelectorAll('a')){a.addEventListener('click',e=>e.preventDefault(),{once:true});a.dispatchEvent(new MouseEvent('click',{cancelable:true}));}if(panel.firstElementChild.textContent)return false;for(const b of panel.querySelectorAll('button'))b.click();return panel.hidden;})()`)
			if !strings.Contains(string(ui), "true") {
				t.Fatalf("layout or print cleanup failed: %s", ui)
			}
			t.Log("layout selection and print notification cleanup passed")
		}
		if app == browserApp {
			takeover := eval(`new Promise(resolve=>{const ws=new WebSocket((location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/api/v1/sessions/'+window.testID+'/signal');window.takeoverSocket=ws;ws.onopen=()=>ws.send(JSON.stringify({csrf_token:window.testCSRF}));ws.onmessage=e=>{if(JSON.parse(e.data).type==='config')setTimeout(()=>resolve({stopped:document.getElementById('session-state').textContent.includes('другой вкладке'),unchanged:window.testPeers.length===window.holdPeers}),5000)}})`)
			var result struct {
				Result struct {
					Value struct {
						Stopped   bool `json:"stopped"`
						Unchanged bool `json:"unchanged"`
					} `json:"value"`
				} `json:"result"`
			}
			if err := json.Unmarshal(takeover, &result); err != nil {
				t.Fatal(err)
			}
			if !result.Result.Value.Stopped || !result.Result.Value.Unchanged {
				t.Fatalf("old client reclaimed session: %s", takeover)
			}
			eval(`window.takeoverSocket.close()`)
			t.Log("second connection took over; first client did not auto-reconnect")
		}
		if app == "terminal" && os.Getenv("MVP_AUTH_EXPIRY_TEST") == "1" && finish == nil {
			db, err := sql.Open("sqlite", filepath.Join(dir, "db"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec("UPDATE auth_sessions SET expires_at=?", time.Now().Add(-time.Second).Unix())
			db.Close()
			if err != nil {
				t.Fatal(err)
			}
			wait(func() bool {
				return strings.Contains(string(eval(`!document.getElementById('login-panel').hidden && document.getElementById('remote-video').srcObject===null`)), "true")
			})
			rejected := eval(`fetch('/api/v1/me').then(r=>r.status)`)
			if !strings.Contains(string(rejected), "401") {
				t.Fatalf("expired cookie accepted: %s", rejected)
			}
			eval(`fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({login:'browser-test',password:'temporary-browser-test'})}).then(r=>r.json()).then(v=>window.testCSRF=v.csrf_token)`)
			t.Log("expired auth closed media, displayed login, rejected API and allowed fresh login")
		}
		if finish != nil {
			eval(`window.mvpSession.close()`)
			finish(launched.Result.Value)
		} else {
			eval(`window.mvpSession.close();fetch('/api/v1/sessions/'+window.testID,{method:'DELETE',headers:{'X-CSRF-Token':window.testCSRF}}).then(r=>r.json())`)
		}
	}
}
