package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseImage(t *testing.T) {
	cases := []struct {
		in                       string
		reg, repo, tag, familiar string
	}{
		{"nginx", "registry-1.docker.io", "library/nginx", "latest", "nginx:latest"},
		{"nginx:1.25", "registry-1.docker.io", "library/nginx", "1.25", "nginx:1.25"},
		{"user/app:v1", "registry-1.docker.io", "user/app", "v1", "user/app:v1"},
		{"ghcr.io/org/app", "ghcr.io", "org/app", "latest", "ghcr.io/org/app:latest"},
		{"localhost:5000/x:y", "localhost:5000", "x", "y", "localhost:5000/x:y"},
		{"docker.io/library/redis:7", "registry-1.docker.io", "library/redis", "7", "redis:7"},
	}
	for _, c := range cases {
		img, err := ParseImage(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if img.Registry != c.reg || img.Repository != c.repo || img.Tag != c.tag || img.Familiar != c.familiar {
			t.Errorf("%s: got %+v", c.in, img)
		}
	}
	img, err := ParseImage("nginx@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || img.Digest == "" || img.Tag != "" {
		t.Fatalf("digest ref: %+v %v", img, err)
	}
	if img.RepoName() != "nginx" {
		t.Fatalf("RepoName = %q", img.RepoName())
	}
}

const indexBody = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:amd64","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:arm64","platform":{"os":"linux","architecture":"arm64"}}]}`

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// newRegistry returns a fake registry that requires a bearer token.
func newRegistry(t *testing.T, requireAuth bool) (*httptest.Server, *int) {
	gets := 0
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && (u != "user" || p != "pass") {
			http.Error(w, "bad creds", 401)
			return
		}
		if r.URL.Query().Get("scope") != "repository:library/nginx:pull" {
			http.Error(w, "bad scope "+r.URL.Query().Get("scope"), 400)
			return
		}
		w.Write([]byte(`{"token":"tok123"}`))
	})
	mux.HandleFunc("/v2/library/nginx/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if requireAuth && r.Header.Get("Authorization") != "Bearer tok123" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:library/nginx:pull"`)
			w.WriteHeader(401)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), mediaTypeOCIIndex) {
			http.Error(w, "missing accept", 400)
			return
		}
		w.Header().Set("Content-Type", mediaTypeOCIIndex)
		w.Header().Set("Docker-Content-Digest", digestOf(indexBody))
		if r.Method == http.MethodHead {
			return
		}
		gets++
		w.Write([]byte(indexBody))
	})
	mux.HandleFunc("/v2/library/single/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", mediaTypeDockerManifest)
		w.Header().Set("Docker-Content-Digest", "sha256:single")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &gets
}

func testClient(srv *httptest.Server) (*Client, Image) {
	host := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient(Options{Insecure: []string{host}, Credentials: StaticCredentials{host: {Username: "user", Password: "pass"}}})
	img := Image{Registry: host, Repository: "library/nginx", Tag: "latest", Familiar: host + "/library/nginx:latest"}
	return c, img
}

func TestResolveHeadMatch(t *testing.T) {
	srv, gets := newRegistry(t, true)
	c, img := testClient(srv)
	r, err := c.Resolve(context.Background(), img, []string{digestOf(indexBody)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Digest != digestOf(indexBody) || !r.IsIndex || *gets != 0 {
		t.Fatalf("unexpected %+v gets=%d", r, *gets)
	}
	if !r.Matches([]string{digestOf(indexBody)}) {
		t.Fatal("should match")
	}
}

func TestResolveFetchesIndexOnMismatch(t *testing.T) {
	srv, gets := newRegistry(t, true)
	c, img := testClient(srv)
	r, err := c.Resolve(context.Background(), img, []string{"sha256:old"})
	if err != nil {
		t.Fatal(err)
	}
	if *gets != 1 || len(r.Children) != 2 {
		t.Fatalf("expected GET for index: gets=%d children=%v", *gets, r.Children)
	}
	if !r.Matches([]string{"sha256:arm64"}) {
		t.Fatal("platform digest should match")
	}
	if r.Matches([]string{"sha256:old"}) {
		t.Fatal("old digest should not match")
	}
	// token should be cached: second call must not need a new challenge
	if _, err := c.Resolve(context.Background(), img, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSingleManifest(t *testing.T) {
	srv, _ := newRegistry(t, false)
	c, img := testClient(srv)
	img.Repository = "library/single"
	r, err := c.Resolve(context.Background(), img, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Digest != "sha256:single" || r.IsIndex {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveBadCreds(t *testing.T) {
	srv, _ := newRegistry(t, true)
	c, img := testClient(srv)
	c.Credentials = StaticCredentials{img.Registry: {Username: "user", Password: "wrong"}}
	if _, err := c.Resolve(context.Background(), img, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveNotFound(t *testing.T) {
	srv, _ := newRegistry(t, false)
	c, img := testClient(srv)
	img.Repository = "library/missing"
	_, err := c.Resolve(context.Background(), img, nil)
	var he *HTTPError
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 error, got %v (%T %v)", err, err, he)
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if s != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" {
		t.Fatalf("got %s %v", s, p)
	}
}
