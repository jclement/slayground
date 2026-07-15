// Package config loads slayground configuration from environment variables
// and an optional YAML file. Environment variables take precedence over the
// file, which takes precedence over built-in defaults.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults for everything except the upstream, which has none on purpose:
// pointing slayground at the right container is the one thing the user must do.
const (
	DefaultListen         = ":80"
	DefaultIdleTimeout    = 30 * time.Minute
	DefaultStartupTimeout = 5 * time.Minute
	DefaultStopTimeout    = 30 * time.Second
	DefaultDockerSocket   = "/var/run/docker.sock"
)

// Route maps a URL path prefix to an upstream base URL.
type Route struct {
	Prefix   string
	Upstream string

	target *url.URL
}

// Target returns the parsed upstream URL. It is only valid after Load.
func (r Route) Target() *url.URL { return r.target }

// Config is the fully resolved slayground configuration.
type Config struct {
	// Listen is the address the proxy listens on.
	Listen string
	// Upstream is the default upstream base URL, used when no route matches.
	// It may be empty if Routes cover all traffic.
	Upstream string
	// Routes are longest-prefix-match overrides for specific paths.
	Routes []Route
	// IdleTimeout is how long the stack may go without meaningful traffic
	// before its containers are stopped.
	IdleTimeout time.Duration
	// StartupTimeout bounds how long a resume waits for containers to
	// become healthy before giving up and forwarding traffic anyway.
	StartupTimeout time.Duration
	// StopTimeout is the per-container grace period when stopping.
	StopTimeout time.Duration
	// IgnoreUserAgents lists case-insensitive substrings; matching requests
	// neither count as activity nor wake a suspended stack.
	IgnoreUserAgents []string
	// IgnorePaths lists path prefixes treated the same way.
	IgnorePaths []string
	// IgnoreContainers lists Compose service names (or full container
	// names) slayground must never stop or start, e.g. a database.
	IgnoreContainers []string
	// ComposeProject overrides Compose project auto-discovery.
	ComposeProject string
	// DockerSocket is the path to the Docker daemon's unix socket.
	DockerSocket string

	defaultTarget *url.URL
}

// DefaultTarget returns the parsed default upstream URL, or nil when no
// default upstream is configured.
func (c *Config) DefaultTarget() *url.URL { return c.defaultTarget }

// fileConfig mirrors Config with YAML tags and string durations.
type fileConfig struct {
	Listen           string   `yaml:"listen"`
	Upstream         string   `yaml:"upstream"`
	IdleTimeout      string   `yaml:"idle_timeout"`
	StartupTimeout   string   `yaml:"startup_timeout"`
	StopTimeout      string   `yaml:"stop_timeout"`
	IgnoreUserAgents []string `yaml:"ignore_user_agents"`
	IgnorePaths      []string `yaml:"ignore_paths"`
	IgnoreContainers []string `yaml:"ignore_containers"`
	ComposeProject   string   `yaml:"compose_project"`
	DockerSocket     string   `yaml:"docker_socket"`
	Routes           []struct {
		Prefix   string `yaml:"prefix"`
		Upstream string `yaml:"upstream"`
	} `yaml:"routes"`
}

// Load builds a Config from the given environment lookup (normally
// os.Getenv), applying the optional SLAYGROUND_CONFIG YAML file first.
func Load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		Listen:         DefaultListen,
		IdleTimeout:    DefaultIdleTimeout,
		StartupTimeout: DefaultStartupTimeout,
		StopTimeout:    DefaultStopTimeout,
		DockerSocket:   DefaultDockerSocket,
	}

	if path := getenv("SLAYGROUND_CONFIG"); path != "" {
		if err := cfg.loadFile(path); err != nil {
			return nil, err
		}
	}
	if err := cfg.loadEnv(getenv); err != nil {
		return nil, err
	}
	if err := cfg.finalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	setStr(&c.Listen, fc.Listen)
	setStr(&c.Upstream, fc.Upstream)
	setStr(&c.ComposeProject, fc.ComposeProject)
	setStr(&c.DockerSocket, fc.DockerSocket)
	if err := setDur(&c.IdleTimeout, fc.IdleTimeout, path+": idle_timeout"); err != nil {
		return err
	}
	if err := setDur(&c.StartupTimeout, fc.StartupTimeout, path+": startup_timeout"); err != nil {
		return err
	}
	if err := setDur(&c.StopTimeout, fc.StopTimeout, path+": stop_timeout"); err != nil {
		return err
	}
	if len(fc.IgnoreUserAgents) > 0 {
		c.IgnoreUserAgents = fc.IgnoreUserAgents
	}
	if len(fc.IgnorePaths) > 0 {
		c.IgnorePaths = fc.IgnorePaths
	}
	if len(fc.IgnoreContainers) > 0 {
		c.IgnoreContainers = fc.IgnoreContainers
	}
	for _, r := range fc.Routes {
		c.Routes = append(c.Routes, Route{Prefix: r.Prefix, Upstream: r.Upstream})
	}
	return nil
}

func (c *Config) loadEnv(getenv func(string) string) error {
	setStr(&c.Listen, getenv("SLAYGROUND_LISTEN"))
	setStr(&c.Upstream, getenv("SLAYGROUND_UPSTREAM"))
	setStr(&c.ComposeProject, getenv("SLAYGROUND_COMPOSE_PROJECT"))
	setStr(&c.DockerSocket, getenv("SLAYGROUND_DOCKER_SOCKET"))
	if err := setDur(&c.IdleTimeout, getenv("SLAYGROUND_IDLE_TIMEOUT"), "SLAYGROUND_IDLE_TIMEOUT"); err != nil {
		return err
	}
	if err := setDur(&c.StartupTimeout, getenv("SLAYGROUND_STARTUP_TIMEOUT"), "SLAYGROUND_STARTUP_TIMEOUT"); err != nil {
		return err
	}
	if err := setDur(&c.StopTimeout, getenv("SLAYGROUND_STOP_TIMEOUT"), "SLAYGROUND_STOP_TIMEOUT"); err != nil {
		return err
	}
	if v := getenv("SLAYGROUND_IGNORE_USER_AGENTS"); v != "" {
		c.IgnoreUserAgents = splitList(v)
	}
	if v := getenv("SLAYGROUND_IGNORE_PATHS"); v != "" {
		c.IgnorePaths = splitList(v)
	}
	if v := getenv("SLAYGROUND_IGNORE_CONTAINERS"); v != "" {
		c.IgnoreContainers = splitList(v)
	}
	if v := getenv("SLAYGROUND_ROUTES"); v != "" {
		routes, err := parseRoutesEnv(v)
		if err != nil {
			return err
		}
		// Env routes replace file routes so the two sources don't merge
		// into something neither of them describes.
		c.Routes = routes
	}
	return nil
}

// parseRoutesEnv parses "prefix=url,prefix=url" pairs, e.g.
// "/api=http://api:3000,/ws=http://ws:9000".
func parseRoutesEnv(v string) ([]Route, error) {
	var routes []Route
	for _, part := range splitList(v) {
		prefix, upstream, ok := strings.Cut(part, "=")
		if !ok || prefix == "" || upstream == "" {
			return nil, fmt.Errorf("SLAYGROUND_ROUTES: %q is not in prefix=url form", part)
		}
		routes = append(routes, Route{Prefix: prefix, Upstream: upstream})
	}
	return routes, nil
}

func (c *Config) finalize() error {
	if c.Upstream == "" && len(c.Routes) == 0 {
		return fmt.Errorf("no upstream configured: set SLAYGROUND_UPSTREAM (e.g. http://app:8080) or define routes")
	}
	if c.Upstream != "" {
		t, err := parseUpstream(c.Upstream)
		if err != nil {
			return fmt.Errorf("upstream: %w", err)
		}
		c.defaultTarget = t
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if !strings.HasPrefix(r.Prefix, "/") {
			return fmt.Errorf("route prefix %q must start with /", r.Prefix)
		}
		t, err := parseUpstream(r.Upstream)
		if err != nil {
			return fmt.Errorf("route %s: %w", r.Prefix, err)
		}
		r.target = t
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"idle timeout", c.IdleTimeout},
		{"startup timeout", c.StartupTimeout},
	} {
		if d.v <= 0 {
			return fmt.Errorf("%s must be positive", d.name)
		}
	}
	if c.StopTimeout < 0 {
		return fmt.Errorf("stop timeout must not be negative")
	}
	return nil
}

func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("URL %q must use http or https", s)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("URL %q has no host", s)
	}
	return u, nil
}

func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func setDur(dst *time.Duration, v, name string) error {
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: invalid duration %q (use forms like 30m, 90s, 1h30m)", name, v)
	}
	*dst = d
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
