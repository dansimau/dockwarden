// Package registry talks to OCI/Docker registries to resolve the digest a
// tag currently points to, without pulling the image.
package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
)

const (
	mediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	mediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
)

var acceptHeader = strings.Join([]string{
	mediaTypeDockerManifestList, mediaTypeOCIIndex, mediaTypeDockerManifest, mediaTypeOCIManifest,
}, ", ")

// Image is a parsed image reference.
type Image struct {
	// Registry is the registry host to contact (e.g. registry-1.docker.io).
	Registry string
	// Repository is the path within the registry (e.g. library/nginx).
	Repository string
	// Tag is the tag (defaults to latest). Empty if the reference is pinned by digest.
	Tag string
	// Digest is set when the reference is pinned to a digest.
	Digest string
	// Familiar is the normalised reference as users write it (docker.io/library/nginx:latest).
	Familiar string
}

// RepoName returns the name used in RepoDigests (e.g. "nginx" or "ghcr.io/foo/bar").
func (i Image) RepoName() string {
	return i.Familiar[:strings.LastIndex(i.Familiar, ":")]
}

// ParseImage normalises an image reference the way Docker does.
func ParseImage(ref string) (Image, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return Image{}, fmt.Errorf("parse image reference %q: %w", ref, err)
	}
	img := Image{
		Registry:   reference.Domain(named),
		Repository: reference.Path(named),
	}
	if img.Registry == "docker.io" {
		img.Registry = "registry-1.docker.io"
	}
	if d, ok := named.(reference.Digested); ok {
		img.Digest = d.Digest().String()
	}
	if t, ok := named.(reference.Tagged); ok {
		img.Tag = t.Tag()
	} else if img.Digest == "" {
		img.Tag = "latest"
	}
	tagged := reference.TagNameOnly(reference.TrimNamed(named))
	fam := reference.FamiliarString(tagged)
	if img.Tag != "" {
		fam = reference.FamiliarName(named) + ":" + img.Tag
	}
	img.Familiar = fam
	return img, nil
}

// Remote describes what a tag currently resolves to in the registry.
type Remote struct {
	// Digest is the digest of the top-level manifest (index or single manifest).
	Digest string
	// MediaType of the top-level manifest.
	MediaType string
	// Children holds platform manifest digests when the top-level object is an index.
	Children []string
	// IsIndex reports whether the top-level object is a manifest list / OCI index.
	IsIndex bool
}

// Matches reports whether any of the given local digests refer to this remote
// manifest (either the index digest or one of its platform manifests).
func (r Remote) Matches(localDigests []string) bool {
	for _, d := range localDigests {
		if d == r.Digest {
			return true
		}
		for _, c := range r.Children {
			if d == c {
				return true
			}
		}
	}
	return false
}

// Client resolves manifests from registries.
type Client struct {
	HTTP        *http.Client
	Credentials CredentialStore
	// Insecure registries use plain HTTP.
	Insecure  map[string]bool
	UserAgent string

	mu     sync.Mutex
	tokens map[string]string // cache key registry+repo -> bearer token
}

// Options configure a Client.
type Options struct {
	Timeout       time.Duration
	SkipTLSVerify bool
	Insecure      []string
	Credentials   CredentialStore
	UserAgent     string
}

// NewClient creates a registry client.
func NewClient(o Options) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.SkipTLSVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // user opt-in
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	creds := o.Credentials
	if creds == nil {
		creds = NoCredentials{}
	}
	insecure := map[string]bool{}
	for _, h := range o.Insecure {
		insecure[h] = true
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "dockwarden"
	}
	return &Client{
		HTTP:        &http.Client{Transport: tr, Timeout: timeout},
		Credentials: creds,
		Insecure:    insecure,
		UserAgent:   ua,
		tokens:      map[string]string{},
	}
}

func (c *Client) scheme(host string) string {
	if c.Insecure[host] {
		return "http"
	}
	return "https"
}

// Resolve returns the digest(s) the image tag currently points to. It issues
// a HEAD request first; if localDigests don't match the top-level digest and
// the manifest is an index, it GETs the index to collect platform digests.
// localDigests may be nil to always fetch the full index.
func (c *Client) Resolve(ctx context.Context, img Image, localDigests []string) (Remote, error) {
	if img.Digest != "" {
		return Remote{Digest: img.Digest}, nil
	}
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", c.scheme(img.Registry), img.Registry, img.Repository, url.PathEscape(img.Tag))

	resp, err := c.do(ctx, http.MethodHead, u, img)
	if err != nil {
		return Remote{}, err
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	var remote Remote
	if resp.StatusCode == http.StatusOK {
		remote.Digest = resp.Header.Get("Docker-Content-Digest")
		remote.MediaType = resp.Header.Get("Content-Type")
		remote.IsIndex = isIndex(remote.MediaType)
		if remote.Digest != "" && localDigests != nil && remote.Matches(localDigests) {
			return remote, nil
		}
		if remote.Digest != "" && !remote.IsIndex {
			return remote, nil
		}
	} else if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
		return Remote{}, httpError(resp, img)
	}

	// Need the body: either HEAD unsupported, digest header missing, or we
	// need platform digests from the index.
	resp, err = c.do(ctx, http.MethodGet, u, img)
	if err != nil {
		return Remote{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Remote{}, httpError(resp, img)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Remote{}, err
	}
	remote.Digest = resp.Header.Get("Docker-Content-Digest")
	if remote.Digest == "" {
		sum := sha256.Sum256(body)
		remote.Digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	remote.MediaType = resp.Header.Get("Content-Type")
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return Remote{}, fmt.Errorf("decode manifest for %s: %w", img.Familiar, err)
	}
	if m.MediaType != "" {
		remote.MediaType = m.MediaType
	}
	remote.IsIndex = isIndex(remote.MediaType) || len(m.Manifests) > 0
	for _, mm := range m.Manifests {
		remote.Children = append(remote.Children, mm.Digest)
	}
	return remote, nil
}

func isIndex(mediaType string) bool {
	mt, _, _ := strings.Cut(mediaType, ";")
	mt = strings.TrimSpace(mt)
	return mt == mediaTypeDockerManifestList || mt == mediaTypeOCIIndex
}

// HTTPError is returned for unexpected registry responses.
type HTTPError struct {
	Status int
	Image  string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("registry returned %d for %s: %s", e.Status, e.Image, strings.TrimSpace(e.Body))
}

func httpError(resp *http.Response, img Image) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &HTTPError{Status: resp.StatusCode, Image: img.Familiar, Body: string(b)}
}

// do performs a request, handling bearer token / basic auth challenges.
func (c *Client) do(ctx context.Context, method, u string, img Image) (*http.Response, error) {
	key := img.Registry + "/" + img.Repository
	c.mu.Lock()
	token := c.tokens[key]
	c.mu.Unlock()

	resp, err := c.send(ctx, method, u, token, Credentials{})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()

	creds, err := c.Credentials.Get(img.Registry)
	if err != nil {
		return nil, fmt.Errorf("credentials for %s: %w", img.Registry, err)
	}
	scheme, params := parseChallenge(challenge)
	switch scheme {
	case "bearer":
		token, err = c.fetchToken(ctx, params, img, creds)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.tokens[key] = token
		c.mu.Unlock()
		return c.send(ctx, method, u, token, Credentials{})
	case "basic":
		if creds.IsZero() {
			return nil, fmt.Errorf("registry %s requires basic auth but no credentials found", img.Registry)
		}
		return c.send(ctx, method, u, "", creds)
	default:
		return nil, fmt.Errorf("registry %s returned 401 with unsupported challenge %q", img.Registry, challenge)
	}
}

func (c *Client) send(ctx context.Context, method, u, bearer string, basic Credentials) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set("User-Agent", c.UserAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if !basic.IsZero() {
		req.SetBasicAuth(basic.Username, basic.Password)
	}
	return c.HTTP.Do(req)
}

var challengeParamRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(h, " ")
	params := map[string]string{}
	for _, m := range challengeParamRe.FindAllStringSubmatch(rest, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	return strings.ToLower(scheme), params
}

func (c *Client) fetchToken(ctx context.Context, params map[string]string, img Image, creds Credentials) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge from %s has no realm", img.Registry)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + img.Repository + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	if creds.IdentityToken != "" {
		// OAuth2 refresh-token flow is required for identity tokens; fall back
		// to anonymous if that is not supported.
		req.Header.Set("Authorization", "Bearer "+creds.IdentityToken)
	} else if !creds.IsZero() {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request to %s: %w", realm, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("token request to %s failed: %d %s", realm, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token from %s: %w", realm, err)
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, nil
	}
	return "", fmt.Errorf("token response from %s contained no token", realm)
}
