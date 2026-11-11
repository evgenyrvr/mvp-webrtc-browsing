package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mvp1/web"
)

func TestAdminFilters(t *testing.T) {
	if os.Getenv("MVP_ADMIN_UI_TEST") != "1" {
		t.Skip("set MVP_ADMIN_UI_TEST=1 for Firefox UI checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"fixture"}}`)
			return
		}
		web.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	start := func(name string, env []string, args ...string) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), env...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	}
	wait := func(check func() bool) {
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	dir, err := os.MkdirTemp("../data", "admin-ui-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	driver := firefoxDriver(t, ctx, dir, port, start, wait)
	driver("Page.navigate", map[string]any{"url": server.URL})
	script := `try {
 const assert=(ok,message)=>{if(!ok)throw Error(message)};
 adminData={users:[],roles:[],applications:[],grants:[],sessions:[]};
 for(let i=1;i<=120;i++){
 adminData.roles.push({id:i,name:'Роль '+i,admin:i%2===0});
 adminData.users.push({id:i,login:'user'+i,enabled:i%2===0,role_ids:[i]});
 adminData.applications.push({id:'site'+i,name:'Сайт '+i,enabled:i%2===0,template:'firefox',start_url:'https://example.org/'+i});
 }
 renderAdminLists();renderGrantOptions();renderRoles([1,120]);
 assert($('users-list').children.length===1,'users must be compact');
 assert($('publications').children.length===1,'profiles must be compact');
 assert($('users-select').options.length===120,'all users selectable');
 $('users-search').value='рОЛЬ 12';$('users-search').dispatchEvent(new Event('input'));
 assert($('users-select').options.length===2,'search by partial role, case insensitive');
 $('users-status').value='false';$('users-status').dispatchEvent(new Event('change'));
 assert($('users-select').disabled && !$('users-list').children.length,'empty blocked filter');
 $('users-search').value='';renderAdminLists();
 assert($('users-select').options.length===60,'blocked users only');
 $('users-status').value='true';renderAdminLists();
 assert($('users-select').options.length===60,'enabled users only');
 $('users-select').value='24';$('users-select').dispatchEvent(new Event('change'));
 assert($('users-list').textContent.includes('user24'),'selection retained');
 document.querySelector('#users-list button').click();
 assert($('user-form').elements.login.value==='user24','edit selected user');
 renderRoles([1,120]);$('user-role-search').value='120';filterUserRoles();
 assert($('user-roles').querySelectorAll('input:checked').length===2,'hidden assignments preserved');
 assert([...$('user-roles').children].filter(x=>!x.hidden).length===1,'role assignment search');
 $('roles-status').value='true';renderAdminLists();
 assert($('roles-select').options.length===60,'admin roles filter');
 $('publications-search').value='example.org/12';$('publications-status').value='true';renderAdminLists();
 assert($('publications-select').options.length===2,'profile URL and enabled filter');
 $('publications-status').value='false';renderAdminLists();
 assert($('publications-select').disabled,'empty profiles');
 $('grant-role-search').value='nonexistent';renderGrantOptions();
 assert($('grant-form').querySelector('button[type=submit]').disabled,'no grant with missing role');
 $('grant-role-search').value='Роль 120';renderGrantOptions();
 assert($('grant-role').value==='120' && !$('grant-form').querySelector('button[type=submit]').disabled,'filtered grant role');
 document.body.dataset.testResult = 'passed';
 } catch(e) { document.body.dataset.testResult = String(e); }`
	encoded, _ := json.Marshal(script)
	driver("Runtime.evaluate", map[string]any{"expression": "(()=>{const s=document.createElement('script');s.textContent=" + string(encoded) + ";document.body.append(s);return true})()"})
	result := driver("Runtime.evaluate", map[string]any{"expression": "document.body.dataset.testResult"})
	if !strings.Contains(string(result), `"value":"passed"`) {
		t.Fatalf("UI checks: %s", result)
	}
}
