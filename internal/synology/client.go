// Package synology is a minimal client for the Synology DSM Web API, covering
// what is needed to redeploy Container Manager projects and containers.
//
// API facts (names, versions, parameters) come from the official DSM Login
// Web API Guide and from community clients that work against DSM 7.2
// Container Manager; see docs/synology-api.md for the sources.
package synology

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dansimau/dockwarden/internal/compose"
)

const (
	apiInfo      = "SYNO.API.Info"
	apiAuth      = "SYNO.API.Auth"
	apiProject   = "SYNO.Docker.Project"
	apiContainer = "SYNO.Docker.Container"
)

// Options configure a Client.
type Options struct {
	URL                string
	Username           string
	Password           string
	InsecureSkipVerify bool
	Timeout            time.Duration
	Logger             *slog.Logger
	// Session is the optional SYNO.API.Auth session name.
	Session string
}

// Client talks to a DSM instance.
type Client struct {
	base     *url.URL
	username string
	password string
	session  string
	http     *http.Client
	log      *slog.Logger

	sid       string
	synotoken string
	versions  map[string]apiInfoEntry
}

type apiInfoEntry struct {
	Path          string `json:"path"`
	MinVersion    int    `json:"minVersion"`
	MaxVersion    int    `json:"maxVersion"`
	RequestFormat string `json:"requestFormat"`
}

// New creates a client. It does not contact DSM until Login.
func New(o Options) (*Client, error) {
	if o.URL == "" {
		return nil, errors.New("synology url is required")
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid synology url %q", o.URL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // user opt-in
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	l := o.Logger
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	return &Client{
		base:     u,
		username: o.Username,
		password: o.Password,
		session:  o.Session,
		http:     &http.Client{Transport: tr, Timeout: timeout},
		log:      l,
	}, nil
}

// APIError is a DSM error response.
type APIError struct {
	API    string
	Method string
	Code   int
}

func (e *APIError) Error() string {
	msg := errorMessages[e.Code]
	if e.API == apiAuth {
		if m, ok := authErrorMessages[e.Code]; ok {
			msg = m
		}
	}
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Sprintf("%s %s failed: error %d (%s)", e.API, e.Method, e.Code, msg)
}

// Common DSM error codes (DSM Login Web API Guide, "Common Error Codes").
var errorMessages = map[int]string{
	100: "unknown error",
	101: "no parameter of API, method or version",
	102: "API does not exist",
	103: "method does not exist",
	104: "version does not support the functionality",
	105: "the logged in session does not have permission (use an administrator account)",
	106: "session timeout",
	107: "session interrupted by duplicated login",
	114: "lost parameters for this API",
	119: "invalid session (SID not found)",
	150: "request source IP does not match the login IP",
}

// SYNO.API.Auth error codes (DSM Login Web API Guide).
var authErrorMessages = map[int]string{
	400: "no such account or incorrect password",
	401: "disabled account",
	402: "denied permission",
	403: "2-factor authentication code required (dockwarden does not support OTP; use an account without 2FA)",
	404: "failed to authenticate 2-factor authentication code",
	406: "enforce to authenticate with 2-factor authentication code",
	407: "blocked IP source",
	408: "expired password cannot change",
	409: "expired password",
	410: "password must be changed",
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// call performs a request against entry.cgi. String values in params are
// sent verbatim; use jsonParams for APIs with requestFormat JSON.
func (c *Client) call(ctx context.Context, api, method string, version int, params url.Values) (json.RawMessage, error) {
	resp, err := c.request(ctx, api, method, version, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return parseEnvelope(api, method, resp.StatusCode, body)
}

func parseEnvelope(api, method string, status int, body []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s %s: unexpected response (HTTP %d): %s", api, method, status, truncate(string(body), 300))
	}
	if !env.Success {
		code := 0
		if env.Error != nil {
			code = env.Error.Code
		}
		return nil, &APIError{API: api, Method: method, Code: code}
	}
	return env.Data, nil
}

func (c *Client) request(ctx context.Context, api, method string, version int, params url.Values) (*http.Response, error) {
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("api", api)
	form.Set("method", method)
	form.Set("version", strconv.Itoa(version))
	if c.sid != "" && api != apiAuth {
		form.Set("_sid", c.sid)
	}
	if c.synotoken != "" {
		form.Set("SynoToken", c.synotoken)
	}
	endpoint := *c.base
	endpoint.Path += "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("User-Agent", "dockwarden")
	if c.synotoken != "" {
		req.Header.Set("X-SYNO-TOKEN", c.synotoken)
	}
	if c.sid != "" {
		req.AddCookie(&http.Cookie{Name: "id", Value: c.sid})
	}
	c.log.Debug("dsm api call", "api", api, "method", method, "version", version)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", api, method, err)
	}
	return resp, nil
}

// jsonParams JSON-encodes each value, as required by APIs whose
// requestFormat is JSON (all SYNO.Docker.* APIs): strings become quoted.
func jsonParams(kv map[string]any) url.Values {
	v := url.Values{}
	for k, val := range kv {
		b, _ := json.Marshal(val)
		v.Set(k, string(b))
	}
	return v
}

// Login discovers API versions and authenticates, obtaining a SID and
// SynoToken.
func (c *Client) Login(ctx context.Context) error {
	if err := c.discover(ctx); err != nil {
		return err
	}
	ver := c.version(apiAuth, 6, 7)
	if ver < 6 {
		return fmt.Errorf("DSM reports %s max version %d; version 6 or newer is required", apiAuth, ver)
	}
	params := url.Values{
		"account":           {c.username},
		"passwd":            {c.password},
		"format":            {"sid"},
		"enable_syno_token": {"yes"},
	}
	if c.session != "" {
		params.Set("session", c.session)
	}
	data, err := c.call(ctx, apiAuth, "login", ver, params)
	if err != nil {
		return err
	}
	var resp struct {
		SID       string `json:"sid"`
		SynoToken string `json:"synotoken"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if resp.SID == "" {
		return errors.New("login response contained no sid")
	}
	c.sid = resp.SID
	c.synotoken = resp.SynoToken
	c.log.Info("logged in to DSM", "url", c.base.String(), "user", c.username)
	return nil
}

// Logout ends the session.
func (c *Client) Logout(ctx context.Context) error {
	if c.sid == "" {
		return nil
	}
	ver := c.version(apiAuth, 6, 7)
	_, err := c.call(ctx, apiAuth, "logout", ver, url.Values{"_sid": {c.sid}})
	c.sid, c.synotoken = "", ""
	return err
}

// LoggedIn reports whether a session exists.
func (c *Client) LoggedIn() bool { return c.sid != "" }

func (c *Client) discover(ctx context.Context) error {
	data, err := c.call(ctx, apiInfo, "query", 1, url.Values{"query": {strings.Join([]string{apiAuth, apiProject, apiContainer}, ",")}})
	if err != nil {
		return fmt.Errorf("query API info: %w", err)
	}
	var m map[string]apiInfoEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("decode API info: %w", err)
	}
	c.versions = m
	if _, ok := m[apiProject]; !ok {
		c.log.Warn("DSM did not report SYNO.Docker.Project; is Container Manager installed?")
	}
	return nil
}

// version returns the highest supported version within [min, max],
// defaulting to min when the API wasn't discovered.
func (c *Client) version(api string, min, max int) int {
	e, ok := c.versions[api]
	if !ok || e.MaxVersion == 0 {
		return min
	}
	v := e.MaxVersion
	if v > max {
		v = max
	}
	return v
}

// Project is a Container Manager project.
type Project struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	SharePath    string   `json:"share_path"`
	Status       string   `json:"status"`
	State        string   `json:"state"`
	Services     []string `json:"services"`
	ContainerIDs []string `json:"containerIds"`
	IsPackage    bool     `json:"is_package"`
	// Content is the project's compose file (returned by `get`).
	Content string `json:"content"`
}

// ComposeName derives the docker compose project name for this DSM project
// the way compose does when run from the project directory: the top-level
// `name:` of the compose file, else COMPOSE_PROJECT_NAME from the
// project's .env (envFile, "" if not readable), else the normalised
// directory basename. The result is normalised like compose does.
func (p Project) ComposeName(envFile string) string {
	return compose.DeriveProjectName([]byte(p.Content), p.Path, envFile)
}

// ListProjects returns all Container Manager projects.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	data, err := c.call(ctx, apiProject, "list", 1, nil)
	if err != nil {
		return nil, err
	}
	// `list` returns an object keyed by project id.
	var byID map[string]Project
	if err := json.Unmarshal(data, &byID); err != nil {
		var arr []Project
		if err2 := json.Unmarshal(data, &arr); err2 != nil {
			return nil, fmt.Errorf("decode project list: %w", err)
		}
		return arr, nil
	}
	out := make([]Project, 0, len(byID))
	for id, p := range byID {
		if p.ID == "" {
			p.ID = id
		}
		out = append(out, p)
	}
	return out, nil
}

// GetProject fetches a project by ID.
func (c *Client) GetProject(ctx context.Context, id string) (*Project, error) {
	data, err := c.call(ctx, apiProject, "get", 1, jsonParams(map[string]any{"id": id}))
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("decode project: %w", err)
	}
	return &p, nil
}

// Project stream methods. These are what the DSM UI itself calls.
const (
	ProjectBuild = "build_stream" // docker compose up --build --no-start
	ProjectStart = "start_stream" // docker compose start
	ProjectStop  = "stop_stream"  // docker compose stop
)

// ProjectAction runs a streaming project action (build_stream, start_stream,
// stop_stream) and consumes the output until DSM closes the stream.
func (c *Client) ProjectAction(ctx context.Context, method, id string) error {
	resp, err := c.request(ctx, apiProject, method, 1, jsonParams(map[string]any{"id": id}))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var last []byte
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	lines := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		lines++
		last = append([]byte(nil), line...)
		c.log.Debug("dsm project stream", "method", method, "line", string(line))
		// A JSON envelope with success=false anywhere in the stream is an error.
		if bytes.HasPrefix(line, []byte("{")) {
			var env envelope
			if json.Unmarshal(line, &env) == nil && env.Error != nil && !env.Success {
				return &APIError{API: apiProject, Method: method, Code: env.Error.Code}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s %s: read stream: %w", apiProject, method, err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: HTTP %d: %s", apiProject, method, resp.StatusCode, truncate(string(last), 300))
	}
	if lines == 1 && bytes.HasPrefix(last, []byte("{")) {
		// Non-streaming style response: validate the envelope.
		if _, err := parseEnvelope(apiProject, method, resp.StatusCode, last); err != nil {
			return err
		}
	}
	return nil
}

// Container actions.
const (
	ContainerStart   = "start"
	ContainerStop    = "stop"
	ContainerRestart = "restart"
)

// ContainerAction runs start/stop/restart on a container by name.
func (c *Client) ContainerAction(ctx context.Context, method, name string) error {
	_, err := c.call(ctx, apiContainer, method, 1, jsonParams(map[string]any{"name": name}))
	return err
}

// ContainerDelete removes a container. With preserveProfile the container's
// settings are kept so a subsequent start recreates it from the (updated)
// image.
func (c *Client) ContainerDelete(ctx context.Context, name string, preserveProfile, force bool) error {
	_, err := c.call(ctx, apiContainer, "delete", 1, jsonParams(map[string]any{"name": name, "preserve_profile": preserveProfile, "force": force}))
	return err
}

// Container is a Container Manager container summary.
type Container struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Image  string `json:"image"`
	Status string `json:"status"`
}

// ListContainers lists containers known to Container Manager.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	data, err := c.call(ctx, apiContainer, "list", 1, jsonParams(map[string]any{"limit": -1, "offset": 0, "type": "all"}))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Containers []Container `json:"containers"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	return resp.Containers, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
