// Package notify sends run summaries to a generic webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// Webhook posts a run report to an HTTP endpoint.
type Webhook struct {
	URL     string
	Method  string
	Headers map[string]string
	// Template renders the body; when empty the report is sent as JSON.
	Template *template.Template
	// On controls when to send: "changes", "errors" or "always".
	On     string
	Client *http.Client
	Logger *slog.Logger
}

// Options configure a Webhook.
type Options struct {
	URL      string
	Method   string
	Headers  map[string]string
	Template string
	On       string
	Timeout  time.Duration
	Logger   *slog.Logger
}

// New creates a webhook notifier; returns nil if no URL is configured.
func New(o Options) (*Webhook, error) {
	if o.URL == "" {
		return nil, nil
	}
	w := &Webhook{URL: o.URL, Method: o.Method, Headers: o.Headers, On: o.On, Logger: o.Logger}
	if w.Method == "" {
		w.Method = http.MethodPost
	}
	if w.On == "" {
		w.On = "changes"
	}
	if w.Logger == nil {
		w.Logger = slog.New(slog.DiscardHandler)
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	w.Client = &http.Client{Timeout: timeout}
	if o.Template != "" {
		t, err := template.New("webhook").Funcs(template.FuncMap{
			"json": func(v any) (string, error) {
				b, err := json.Marshal(v)
				return string(b), err
			},
			"join": strings.Join,
		}).Parse(o.Template)
		if err != nil {
			return nil, fmt.Errorf("parse notify.webhook.template: %w", err)
		}
		w.Template = t
	}
	return w, nil
}

// Report is the payload sent to the webhook.
type Report struct {
	Host        string    `json:"host"`
	StartedAt   time.Time `json:"started_at"`
	Duration    string    `json:"duration"`
	DryRun      bool      `json:"dry_run"`
	MonitorOnly bool      `json:"monitor_only"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Updated     []Item    `json:"updated"`
	Pending     []Item    `json:"pending"`
	Failed      []Item    `json:"failed"`
	Scanned     int       `json:"scanned"`
	Errors      []string  `json:"errors"`
}

// Item describes one container in the report.
type Item struct {
	Container string   `json:"container"`
	Kind      string   `json:"kind"`
	Image     string   `json:"image"`
	Project   string   `json:"project,omitempty"`
	Service   string   `json:"service,omitempty"`
	OldDigest string   `json:"old_digest,omitempty"`
	NewDigest string   `json:"new_digest,omitempty"`
	Bases     []string `json:"base_images,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// ShouldSend applies the On policy.
func (w *Webhook) ShouldSend(r *Report) bool {
	hasChanges := len(r.Updated) > 0 || len(r.Pending) > 0
	hasErrors := len(r.Failed) > 0 || len(r.Errors) > 0
	switch w.On {
	case "always":
		return true
	case "errors":
		return hasErrors
	default:
		return hasChanges || hasErrors
	}
}

// Send delivers the report if the policy says so.
func (w *Webhook) Send(ctx context.Context, r *Report) error {
	if w == nil || !w.ShouldSend(r) {
		return nil
	}
	var body bytes.Buffer
	contentType := "application/json"
	if w.Template != nil {
		if err := w.Template.Execute(&body, r); err != nil {
			return fmt.Errorf("render webhook template: %w", err)
		}
		contentType = "text/plain; charset=utf-8"
		if json.Valid(body.Bytes()) {
			contentType = "application/json"
		}
	} else if err := json.NewEncoder(&body).Encode(r); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, w.Method, w.URL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "dockwarden")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	w.Logger.Info("notification sent", "url", w.URL, "status", resp.StatusCode)
	return nil
}
