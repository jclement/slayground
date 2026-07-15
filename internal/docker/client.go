// Package docker is a minimal Docker Engine API client covering exactly what
// slayground needs: discover its own Compose project and list/stop/start the
// containers in it. Talking to the API directly over the unix socket keeps
// the binary tiny compared to pulling in the full Docker SDK.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultSocket is the standard Docker daemon socket path.
const DefaultSocket = "/var/run/docker.sock"

// Compose- and slayground-specific container labels.
const (
	LabelProject = "com.docker.compose.project"
	LabelOneOff  = "com.docker.compose.oneoff"
	// LabelExclude marks a container slayground must never stop or start,
	// e.g. a database that should keep running while the stack sleeps.
	LabelExclude = "slayground.exclude"
)

// Client talks to the Docker Engine API over a unix socket.
type Client struct {
	hc *http.Client
}

// NewClient returns a Client for the daemon at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{hc: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}}
}

// Container is a subset of a /containers/json list entry.
type Container struct {
	ID     string
	Name   string
	State  string // "running", "exited", "created", ...
	Labels map[string]string
}

// Health is a container healthcheck summary.
type Health struct {
	Status string `json:"Status"` // "starting", "healthy", "unhealthy"
}

// InspectState is the State block of a container inspect result.
type InspectState struct {
	Running  bool    `json:"Running"`
	ExitCode int     `json:"ExitCode"`
	Health   *Health `json:"Health"`
}

// InspectResult is a subset of a container inspect response.
type InspectResult struct {
	ID     string       `json:"Id"`
	Name   string       `json:"Name"`
	State  InspectState `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Ping checks connectivity to the daemon.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil)
}

// ListProject returns all containers (running or not) labeled as belonging
// to the given Compose project.
func (c *Client) ListProject(ctx context.Context, project string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{
		"label": {LabelProject + "=" + project},
	})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	path := "/containers/json?all=1&filters=" + url.QueryEscape(string(filters))
	if err := c.do(ctx, http.MethodGet, path, &raw); err != nil {
		return nil, err
	}
	containers := make([]Container, 0, len(raw))
	for _, r := range raw {
		name := r.ID
		if len(r.Names) > 0 {
			name = strings.TrimPrefix(r.Names[0], "/")
		}
		containers = append(containers, Container{
			ID:     r.ID,
			Name:   name,
			State:  r.State,
			Labels: r.Labels,
		})
	}
	return containers, nil
}

// Inspect returns details for a container referenced by ID or name.
func (c *Client) Inspect(ctx context.Context, ref string) (*InspectResult, error) {
	var res InspectResult
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(ref)+"/json", &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Stop stops a container, giving it the specified grace period. Stopping an
// already-stopped container is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	path := "/containers/" + url.PathEscape(id) + "/stop"
	if timeout > 0 {
		path += fmt.Sprintf("?t=%d", int(math.Ceil(timeout.Seconds())))
	}
	return c.do(ctx, http.MethodPost, path, nil)
}

// Start starts a container. Starting an already-running container is not an
// error.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
}

// DiscoverSelf inspects the container this process runs in (the hostname is
// the container ID under Docker) and returns its Compose project and full
// container ID.
func (c *Client) DiscoverSelf(ctx context.Context) (project, id string, err error) {
	host, err := os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("hostname: %w", err)
	}
	ins, err := c.Inspect(ctx, host)
	if err != nil {
		return "", "", fmt.Errorf("inspecting own container %q: %w", host, err)
	}
	project = ins.Config.Labels[LabelProject]
	if project == "" {
		return "", "", fmt.Errorf("container %q has no %s label; is slayground part of a compose stack?", host, LabelProject)
	}
	return project, ins.ID, nil
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Message != "" {
			msg = apiErr.Message
		}
		return fmt.Errorf("docker: %s %s: %s (status %d)", method, path, msg, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
