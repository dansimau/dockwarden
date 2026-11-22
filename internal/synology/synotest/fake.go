package synotest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Project mirrors the fields of a DSM Container Manager project.
type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Content string `json:"content,omitempty"`
}

// Container mirrors a DSM container summary.
type Container struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
}

// DSM is an httptest server emulating the subset of the DSM Web API that
// dockwarden uses. Login requires account "admin" / password "pw".
type DSM struct {
	t        *testing.T
	URL      string
	mu       sync.Mutex
	calls    []string // "api/method"
	params   []map[string]string
	Projects map[string]Project
	Fail     map[string]int // "api/method" -> error code
}

// New starts a fake DSM; it is closed when the test ends.
func New(t *testing.T) *DSM {
	f := &DSM{t: t, Projects: map[string]Project{}, Fail: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

func (f *DSM) write(w http.ResponseWriter, data any) {
	json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (f *DSM) fail_(w http.ResponseWriter, code int) {
	json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]any{"code": code}})
}

func (f *DSM) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/webapi/entry.cgi" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	api, method, version := r.Form.Get("api"), r.Form.Get("method"), r.Form.Get("version")
	key := api + "/" + method
	params := map[string]string{}
	for k := range r.Form {
		params[k] = r.Form.Get(k)
	}
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.params = append(f.params, params)
	code := f.Fail[key]
	f.mu.Unlock()
	if code != 0 {
		f.fail_(w, code)
		return
	}
	switch key {
	case "SYNO.API.Info/query":
		f.write(w, map[string]any{
			"SYNO.API.Auth":         map[string]any{"path": "entry.cgi", "minVersion": 1, "maxVersion": 7},
			"SYNO.Docker.Project":   map[string]any{"path": "entry.cgi", "minVersion": 1, "maxVersion": 1, "requestFormat": "JSON"},
			"SYNO.Docker.Container": map[string]any{"path": "entry.cgi", "minVersion": 1, "maxVersion": 1, "requestFormat": "JSON"},
		})
		return
	case "SYNO.API.Auth/login":
		if version != "7" {
			f.t.Errorf("login version %s", version)
		}
		if r.Form.Get("account") != "admin" || r.Form.Get("passwd") != "pw" {
			f.fail_(w, 400)
			return
		}
		if r.Form.Get("format") != "sid" || r.Form.Get("enable_syno_token") != "yes" {
			f.t.Errorf("unexpected login params %v", r.Form)
		}
		f.write(w, map[string]any{"sid": "SID123", "synotoken": "TOK"})
		return
	case "SYNO.API.Auth/logout":
		f.write(w, map[string]any{})
		return
	}
	// everything else requires a session (SID + SynoToken)
	if r.Form.Get("_sid") != "SID123" {
		f.fail_(w, 119)
		return
	}
	if r.Header.Get("X-SYNO-TOKEN") != "TOK" {
		f.fail_(w, 105)
		return
	}
	if version != "1" {
		f.fail_(w, 104)
		return
	}
	switch key {
	case "SYNO.Docker.Project/list":
		// Like DSM, list returns project summaries without the compose content.
		summaries := map[string]Project{}
		f.mu.Lock()
		for id, p := range f.Projects {
			p.Content = ""
			summaries[id] = p
		}
		f.mu.Unlock()
		f.write(w, summaries)
	case "SYNO.Docker.Project/get":
		id := r.Form.Get("id")
		if !strings.HasPrefix(id, `"`) {
			f.t.Errorf("id not JSON quoted: %s", id)
		}
		p, ok := f.Projects[strings.Trim(id, `"`)]
		if !ok {
			f.fail_(w, 100)
			return
		}
		f.write(w, p)
	case "SYNO.Docker.Project/build_stream", "SYNO.Docker.Project/start_stream", "SYNO.Docker.Project/stop_stream":
		id := strings.Trim(r.Form.Get("id"), `"`)
		p, ok := f.Projects[id]
		if !ok {
			f.fail_(w, 100)
			return
		}
		fmt.Fprintln(w, "Building web")
		fmt.Fprintln(w, "web  Built")
		switch method {
		case "start_stream":
			p.Status = "RUNNING"
		case "stop_stream":
			p.Status = "STOPPED"
		}
		f.mu.Lock()
		f.Projects[id] = p
		f.mu.Unlock()
	case "SYNO.Docker.Container/start", "SYNO.Docker.Container/stop", "SYNO.Docker.Container/restart", "SYNO.Docker.Container/delete":
		name := r.Form.Get("name")
		if !strings.HasPrefix(name, `"`) {
			f.t.Errorf("name not JSON quoted: %s", name)
		}
		if method == "delete" && (r.Form.Get("preserve_profile") != "true" || r.Form.Get("force") != "false") {
			f.t.Errorf("delete params %v", r.Form)
		}
		f.write(w, map[string]any{"name": strings.Trim(name, `"`)})
	case "SYNO.Docker.Container/list":
		if r.Form.Get("limit") != "-1" || r.Form.Get("type") != `"all"` {
			f.t.Errorf("list params %v", r.Form)
		}
		f.write(w, map[string]any{"containers": []Container{{Name: "web", Image: "nginx:1", Status: "running"}}, "total": 1})
	default:
		f.fail_(w, 103)
	}
}

// Calls returns the "api/method" pairs received so far.
func (f *DSM) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
