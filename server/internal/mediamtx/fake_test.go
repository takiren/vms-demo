package mediamtx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

const (
	apiUser = "vms-api"
	apiPass = "control-api-password"
)

// fakeMTX imitates the MediaMTX v3 config/paths endpoints.
type fakeMTX struct {
	mu        sync.Mutex
	paths     map[string]mediamtx.PathConf
	calls     []string
	failNames map[string]bool // add/replace/delete of these fail with 400
	down      bool
}

func newFake(t *testing.T) (*fakeMTX, *httptest.Server) {
	t.Helper()
	f := &fakeMTX{paths: map[string]mediamtx.PathConf{}, failNames: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMTX) set(p mediamtx.PathConf) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths[p.Name] = p
}

func (f *fakeMTX) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeMTX) snapshot() map[string]mediamtx.PathConf {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]mediamtx.PathConf{}
	for k, v := range f.paths {
		out[k] = v
	}
	return out
}

func (f *fakeMTX) writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": msg})
}

func (f *fakeMTX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		f.writeErr(w, http.StatusInternalServerError, "down")
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != apiUser || p != apiPass {
		f.writeErr(w, http.StatusUnauthorized, "authentication error")
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/v3/config/paths/list":
		names := make([]string, 0, len(f.paths))
		for n := range f.paths {
			names = append(names, n)
		}
		sort.Strings(names)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		per, _ := strconv.Atoi(r.URL.Query().Get("itemsPerPage"))
		if per == 0 {
			per = 100
		}
		// Real items carry many more fields; the client must ignore them.
		items := []map[string]any{}
		for i := page * per; i < len(names) && i < (page+1)*per; i++ {
			p := f.paths[names[i]]
			items = append(items, map[string]any{
				"name": p.Name, "source": p.Source, "sourceOnDemand": p.SourceOnDemand, "record": p.Record,
				"recordFormat": "fmp4", "maxReaders": 0,
			})
		}
		pageCount := (len(names) + per - 1) / per
		_ = json.NewEncoder(w).Encode(map[string]any{"itemCount": len(names), "pageCount": pageCount, "items": items})
	case strings.HasPrefix(path, "/v3/config/paths/add/") && r.Method == http.MethodPost:
		name := strings.TrimPrefix(path, "/v3/config/paths/add/")
		if f.failNames[name] {
			f.writeErr(w, http.StatusBadRequest, "invalid source 'rtsp://user:topsecret@cam/x'")
			return
		}
		if _, ok := f.paths[name]; ok {
			f.writeErr(w, http.StatusBadRequest, "path already exists")
			return
		}
		var p mediamtx.PathConf
		_ = json.NewDecoder(r.Body).Decode(&p)
		p.Name = name
		f.paths[name] = p
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case strings.HasPrefix(path, "/v3/config/paths/replace/") && r.Method == http.MethodPost:
		name := strings.TrimPrefix(path, "/v3/config/paths/replace/")
		if _, ok := f.paths[name]; !ok {
			f.writeErr(w, http.StatusNotFound, "path not found")
			return
		}
		var p mediamtx.PathConf
		_ = json.NewDecoder(r.Body).Decode(&p)
		p.Name = name
		f.paths[name] = p
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	case strings.HasPrefix(path, "/v3/config/paths/delete/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/v3/config/paths/delete/")
		if _, ok := f.paths[name]; !ok {
			f.writeErr(w, http.StatusNotFound, "path not found")
			return
		}
		delete(f.paths, name)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	default:
		f.writeErr(w, http.StatusNotFound, "not found")
	}
}
