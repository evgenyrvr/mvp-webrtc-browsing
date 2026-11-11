package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func testDesktop(t *testing.T, ctx context.Context, eval func(string) json.RawMessage) {
	t.Helper()
	value := func(expr string) any {
		t.Helper()
		var result struct{ Result struct{ Value any } }
		raw := eval(expr)
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result.Result.Value
	}
	waitFor := func(expr string) {
		t.Helper()
		until := time.Now().Add(45 * time.Second)
		for value(expr) != true {
			if time.Now().After(until) {
				t.Fatalf("desktop timeout: %s; diagnostics: %s", expr, eval(`({message:document.getElementById('message').textContent,path:location.pathname,frames:[...document.querySelectorAll('#session-frames iframe')].map(f=>({url:f.src,state:f.contentDocument?.getElementById('session-state')?.textContent}))})`))
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	eval(`window.open=()=>{throw Error('Unexpected browser tab')};refreshIdentity()`)
	waitFor(`!!document.querySelector('[data-app="falkon"] button')`)
	eval(`document.querySelector('[data-app="falkon"] button').click()`)
	waitFor(`document.querySelector('#session-frames iframe')?.contentDocument?.getElementById('remote-video')?.videoWidth===1280`)
	first := value(`location.pathname.split('/').pop()`).(string)
	eval(`window.firstFrame=document.querySelector('#session-frames iframe');document.getElementById('desktop-catalog').click()`)
	waitFor(`location.pathname==='/' && !document.getElementById('workspace').hidden`)
	eval(`document.querySelector('[data-app="terminal"] button').click()`)
	waitFor(`document.querySelectorAll('#session-frames iframe').length===2 && [...document.querySelectorAll('#session-frames iframe')].filter(f=>!f.hidden)[0]?.contentDocument?.getElementById('remote-video')?.videoWidth===1280`)
	second := value(`location.pathname.split('/').pop()`).(string)
	if first == second {
		t.Fatal("expected independent applications")
	}
	if value(`document.querySelectorAll('#session-tabs button').length===2 && window.firstFrame.isConnected && window.firstFrame.hidden`) != true {
		t.Fatal("missing retained browser")
	}
	// Type into the selected terminal through the actual input event handlers.
	eval(`(()=>{const frame=[...document.querySelectorAll('#session-frames iframe')].find(f=>!f.hidden);const stage=frame.contentDocument.getElementById('remote-stage');stage.focus();const text='touch /tmp/mvp1-desktop-proof';const codes=[...text].map(c=>/[a-z]/.test(c)?'Key'+c.toUpperCase():/[0-9]/.test(c)?'Digit'+c:({' ':'Space','/':'Slash','-':'Minus'})[c]);codes.push('Enter');for(const code of codes)for(const type of ['keydown','keyup'])stage.dispatchEvent(new frame.contentWindow.KeyboardEvent(type,{code,bubbles:true,cancelable:true}));})()`)
	until := time.Now().Add(5 * time.Second)
	for exec.CommandContext(ctx, "docker", "exec", "mvp1-browser-"+second, "test", "-f", "/tmp/mvp1-desktop-proof").Run() != nil {
		if time.Now().After(until) {
			t.Fatal("terminal input failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Log("two applications in one browser tab; terminal input verified")
	// Longer than the worker's 90-second disconnected idle limit.
	rounds := 5
	if os.Getenv("MVP_DESKTOP_TEST") == "quick" {
		rounds = 0
	}
	for i := 0; i < rounds; i++ {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Second):
		}
		if i == 2 {
			t.Log("hidden browser remains connected; checking beyond idle timeout")
		}
	}
	eval(fmt.Sprintf(`document.querySelector('[data-session="%s"]').click()`, first))
	waitFor(`!window.firstFrame.hidden && window.firstFrame.contentDocument.getElementById('remote-video').videoWidth===1280`)
	if value(`document.querySelector('#session-frames iframe')===window.firstFrame`) != true {
		t.Fatal("switch recreated browser")
	}
	if err := exec.CommandContext(ctx, "docker", "exec", "mvp1-browser-"+second, "test", "-f", "/tmp/mvp1-desktop-proof").Run(); err != nil {
		t.Fatal("terminal state lost", err)
	}
	t.Logf("switch preserved frames and terminal state after %d seconds", rounds*20)
	// Browser history must switch views without losing retained frames.
	eval(`history.back()`)
	waitFor(fmt.Sprintf(`location.pathname==='/sessions/%s' && document.querySelector('[data-session="%s"]').getAttribute('aria-pressed')==='true'`, second, second))
	eval(`history.forward()`)
	waitFor(fmt.Sprintf(`location.pathname==='/sessions/%s' && !window.firstFrame.hidden`, first))
	// Stop current browser; remaining terminal is selected automatically.
	eval(`window.firstFrame.contentDocument.getElementById('stop-app').click()`)
	waitFor(fmt.Sprintf(`location.pathname==='/sessions/%s' && document.querySelectorAll('#session-frames iframe').length===1 && !document.querySelector('#session-frames iframe').hidden && !document.getElementById('desktop-view').hidden`, second))
	dimensions := eval(`(()=>{const f=document.querySelector('#session-frames iframe');const r=f.contentDocument.getElementById('remote-stage').getBoundingClientRect();return {width:r.width,height:r.height,viewport:innerHeight}})()`)
	t.Logf("desktop space: %s", dimensions)
	if value(`(()=>{const f=document.querySelector('#session-frames iframe');return f.contentDocument.getElementById('remote-stage').getBoundingClientRect().height>innerHeight*0.5})()`) != true {
		t.Fatal("remote viewport too small")
	}
	eval(`document.querySelector('#session-frames iframe').contentDocument.getElementById('stop-app').click()`)
	waitFor(`location.pathname==='/' && document.querySelectorAll('#session-frames iframe').length===0`)
	if strings.Contains(string(eval(`document.getElementById('message').textContent`)), "Unexpected") {
		t.Fatal("unexpected new tab")
	}
	t.Log("Back/Forward, automatic selection after stop and return to catalog passed")
}
