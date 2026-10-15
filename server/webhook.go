package main

// webhook_url: a JSON POST for every kill / would-kill, the server
// counterpart of the desktop apps' notifications (Slack-compatible "text"
// field included).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

// httpClient returns a client that uses the configured proxy (a service does
// not inherit HTTPS_PROXY from an admin's shell), falling back to the
// environment's proxy settings.
func httpClient(cfg Config, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.HTTPSProxy != "" {
		if u, err := url.Parse(cfg.HTTPSProxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

type webhookPayload struct {
	Event
	Host    string `json:"host"`
	Version string `json:"version"`
	Text    string `json:"text"`
}

func eventText(host string, ev Event) string {
	switch ev.Action {
	case "killed":
		return fmt.Sprintf("Hotfix on %s killed %s (PID %d): %.0f%% CPU for %.0fs", host, ev.Name, ev.PID, ev.CPU, ev.HotFor)
	case "would_kill":
		return fmt.Sprintf("Hotfix on %s would kill %s (PID %d): %.0f%% CPU for %.0fs (monitor mode)", host, ev.Name, ev.PID, ev.CPU, ev.HotFor)
	case "kill_failed":
		return fmt.Sprintf("Hotfix on %s could not kill %s (PID %d): %s", host, ev.Name, ev.PID, ev.Error)
	default:
		return fmt.Sprintf("Hotfix on %s stopped killing %s: it keeps restarting", host, ev.Name)
	}
}

// sendWebhook posts ev in the background; failures are logged, never retried.
func sendWebhook(cfg Config, ev Event) {
	if cfg.WebhookURL == "" {
		return
	}
	host, _ := os.Hostname()
	body, err := json.Marshal(webhookPayload{Event: ev, Host: host, Version: version, Text: eventText(host, ev)})
	if err != nil {
		return
	}
	go func() {
		resp, err := httpClient(cfg, 10*time.Second).Post(cfg.WebhookURL, "application/json", bytes.NewReader(body))
		if err != nil {
			logf("webhook: %v", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			logf("webhook: %s answered %s", cfg.WebhookURL, resp.Status)
		}
	}()
}
