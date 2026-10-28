// Package gluetun talks to gluetun's control server: where the tunnel exits, and a reconnect that cycles the VPN and
// waits for a new public address. Ported from docker-log-watchdog. gluetun is an optional provider for the agent: with
// no URL configured nothing here runs.
package gluetun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is one gluetun control server.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	Log     func(string, ...any)

	Attempts      int           // reconnect cycles before giving up (3)
	StatusTimeout time.Duration // wait for the tunnel to report stopped/running (30s)
	IPTimeout     time.Duration // wait for a different public address (90s)
	Poll          time.Duration // between polls (2s)
}

// Exit is what /v1/publicip/ip reports.
type Exit struct {
	IP      string `json:"public_ip"`
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
}

// String is the card's exit text: "France, Paris", "Spain", or "" when unknown.
func (e Exit) String() string {
	switch {
	case e.Country != "" && e.City != "":
		return e.Country + ", " + e.City
	case e.Country != "":
		return e.Country
	default:
		return ""
	}
}

func (c *Client) defaults() {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if c.Attempts <= 0 {
		c.Attempts = 3
	}
	if c.StatusTimeout <= 0 {
		c.StatusTimeout = 30 * time.Second
	}
	if c.IPTimeout <= 0 {
		c.IPTimeout = 90 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 2 * time.Second
	}
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	c.defaults()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-API-Key", c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gluetun %s %s: status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// PublicIP asks where the tunnel exits. An empty IP means the tunnel is down or not yet resolved.
func (c *Client) PublicIP(ctx context.Context) (Exit, error) {
	var e Exit
	err := c.do(ctx, http.MethodGet, "/v1/publicip/ip", nil, &e)
	return e, err
}

// Status returns "running" or "stopped".
func (c *Client) Status(ctx context.Context) (string, error) {
	var s struct {
		Status string `json:"status"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/vpn/status", nil, &s)
	return s.Status, err
}

// SetStatus asks for "running" or "stopped".
func (c *Client) SetStatus(ctx context.Context, status string) error {
	b, _ := json.Marshal(map[string]string{"status": status})
	return c.do(ctx, http.MethodPut, "/v1/vpn/status", b, nil)
}

// Reconnect cycles the tunnel until gluetun reports a public address different from the one before, and returns it.
// An empty address before means any address counts as new.
func (c *Client) Reconnect(ctx context.Context) (string, error) {
	c.defaults()
	before := ""
	if e, err := c.PublicIP(ctx); err == nil {
		before = e.IP
	}
	c.Log("gluetun: reconnecting, address before %q", before)
	for attempt := 1; attempt <= c.Attempts; attempt++ {
		if err := c.SetStatus(ctx, "stopped"); err != nil {
			return "", fmt.Errorf("stop: %w", err)
		}
		c.waitStatus(ctx, "stopped")
		if err := c.SetStatus(ctx, "running"); err != nil {
			return "", fmt.Errorf("start: %w", err)
		}
		if err := c.waitStatus(ctx, "running"); err != nil {
			c.Log("gluetun: attempt %d/%d, tunnel did not come back: %v", attempt, c.Attempts, err)
			continue
		}
		ip, err := c.waitNewIP(ctx, before)
		if err != nil {
			c.Log("gluetun: attempt %d/%d: %v", attempt, c.Attempts, err)
			continue
		}
		c.Log("gluetun: reconnected, new address %s", ip)
		return ip, nil
	}
	return "", fmt.Errorf("no new public address after %d attempts", c.Attempts)
}

func (c *Client) waitStatus(ctx context.Context, want string) error {
	deadline := time.Now().Add(c.StatusTimeout)
	for time.Now().Before(deadline) {
		if s, err := c.Status(ctx); err == nil && s == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.Poll):
		}
	}
	return fmt.Errorf("timed out waiting for status %q", want)
}

func (c *Client) waitNewIP(ctx context.Context, before string) (string, error) {
	deadline := time.Now().Add(c.IPTimeout)
	for time.Now().Before(deadline) {
		if e, err := c.PublicIP(ctx); err == nil && e.IP != "" && e.IP != before {
			return e.IP, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(c.Poll):
		}
	}
	return "", fmt.Errorf("timed out waiting for an address other than %q", before)
}
