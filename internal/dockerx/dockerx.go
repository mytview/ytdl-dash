// Package dockerx is the little of the Docker Engine API the agent needs, over the unix socket with net/http and no SDK:
// inspect a container (health, state, start time), restart it, and exec a command in it, detached or with its output.
// Everything here is optional for the agent: without a socket the agent still observes and reports, it just cannot
// start runs or restart anything.
package dockerx

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one Docker daemon.
type Client struct {
	Socket string // default /var/run/docker.sock
	http   *http.Client
}

func (c *Client) client() *http.Client {
	if c.http == nil {
		sock := c.Socket
		if sock == "" {
			sock = "/var/run/docker.sock"
		}
		c.http = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}, Timeout: 5 * time.Minute}
	}
	return c.http
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("docker %s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

// Available reports whether the socket answers.
func (c *Client) Available(ctx context.Context) bool {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Info reads the daemon's own hostname: the machine's name, which is what the card wants, where the agent's own
// hostname is just its container id.
func (c *Client) Info(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/info", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var raw struct {
		Name string `json:"Name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", err
	}
	return raw.Name, nil
}

// State is what the card shows about the container.
type State struct {
	Status    string // running, exited...
	Health    string // healthy, unhealthy, starting, or "" without a healthcheck
	StartedAt time.Time
	Image     string
}

// Inspect reads a container's state by name or id.
func (c *Client) Inspect(ctx context.Context, name string) (State, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return State{}, err
	}
	defer resp.Body.Close()
	var raw struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		State struct {
			Status    string `json:"Status"`
			StartedAt string `json:"StartedAt"`
			Health    *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return State{}, err
	}
	s := State{Status: raw.State.Status, Image: raw.Config.Image}
	if raw.State.Health != nil {
		s.Health = raw.State.Health.Status
	}
	s.StartedAt, _ = time.Parse(time.RFC3339Nano, raw.State.StartedAt)
	return s, nil
}

// Restart restarts a container, giving it timeout seconds to stop.
func (c *Client) Restart(ctx context.Context, name string, timeout int) error {
	resp, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/containers/%s/restart?t=%d", url.PathEscape(name), timeout), nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type execCreate struct {
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Cmd          []string `json:"Cmd"`
	User         string   `json:"User,omitempty"`
}

func (c *Client) execID(ctx context.Context, name string, cmd []string, attach bool, user string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/exec", execCreate{AttachStdout: attach, AttachStderr: attach, Cmd: cmd, User: user})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ExecDetached starts a command in the container and returns at once. Used to start a run: its output goes wherever
// the command sends it (deploy/instance/cron.sh appends to the cron log, which the agent is tailing).
func (c *Client) ExecDetached(ctx context.Context, name string, cmd []string, user string) error {
	id, err := c.execID(ctx, name, cmd, false, user)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/exec/"+id+"/start", map[string]bool{"Detach": true})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ExecOutput runs a command in the container and returns what it printed (stdout and stderr, in order). A non-zero
// exit code is an error carrying the output, so a missing file or a failed command is never mistaken for content.
func (c *Client) ExecOutput(ctx context.Context, name string, cmd []string) (string, error) {
	id, err := c.execID(ctx, name, cmd, true, "")
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodPost, "/exec/"+id+"/start", map[string]bool{"Detach": false, "Tty": false})
	if err != nil {
		return "", err
	}
	out := demux(resp.Body)
	resp.Body.Close()
	ins, err := c.do(ctx, http.MethodGet, "/exec/"+id+"/json", nil)
	if err != nil {
		return out, nil
	}
	defer ins.Body.Close()
	var st struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if json.NewDecoder(ins.Body).Decode(&st) == nil && !st.Running && st.ExitCode != 0 {
		return out, fmt.Errorf("%s exited %d: %s", strings.Join(cmd, " "), st.ExitCode, strings.TrimSpace(out))
	}
	return out, nil
}

// demux reads Docker's multiplexed stream: 8-byte frame headers (type, 3 zero bytes, big-endian length), then payload.
func demux(r io.Reader) string {
	var out strings.Builder
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return out.String()
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if n > 16<<20 {
			return out.String()
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			out.Write(buf)
			return out.String()
		}
		out.Write(buf)
	}
}
