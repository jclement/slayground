package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"SLAYGROUND_UPSTREAM": "http://app:8080",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":80" {
		t.Errorf("Listen = %q, want :80", cfg.Listen)
	}
	if cfg.IdleTimeout != 30*time.Minute {
		t.Errorf("IdleTimeout = %v, want 30m", cfg.IdleTimeout)
	}
	if cfg.StartupTimeout != 5*time.Minute {
		t.Errorf("StartupTimeout = %v, want 5m", cfg.StartupTimeout)
	}
	if cfg.StopTimeout != 30*time.Second {
		t.Errorf("StopTimeout = %v, want 30s", cfg.StopTimeout)
	}
	if cfg.DockerSocket != "/var/run/docker.sock" {
		t.Errorf("DockerSocket = %q", cfg.DockerSocket)
	}
	if got := cfg.DefaultTarget().String(); got != "http://app:8080" {
		t.Errorf("DefaultTarget = %q", got)
	}
}

func TestMissingUpstream(t *testing.T) {
	_, err := Load(envFrom(nil))
	if err == nil || !strings.Contains(err.Error(), "no upstream configured") {
		t.Fatalf("err = %v, want no-upstream error", err)
	}
}

func TestEnvParsing(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"SLAYGROUND_UPSTREAM":           "http://web:3000",
		"SLAYGROUND_LISTEN":             ":8080",
		"SLAYGROUND_IDLE_TIMEOUT":       "5m",
		"SLAYGROUND_STARTUP_TIMEOUT":    "90s",
		"SLAYGROUND_STOP_TIMEOUT":       "10s",
		"SLAYGROUND_IGNORE_USER_AGENTS": "UptimeRobot, Pingdom ,",
		"SLAYGROUND_IGNORE_PATHS":       "/health,/ping",
		"SLAYGROUND_IGNORE_CONTAINERS":  "db, cache",
		"SLAYGROUND_ROUTES":             "/api=http://api:9000,/ws=http://ws:9001",
		"SLAYGROUND_COMPOSE_PROJECT":    "myproj",
		"SLAYGROUND_DOCKER_SOCKET":      "/tmp/docker.sock",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" || cfg.IdleTimeout != 5*time.Minute ||
		cfg.StartupTimeout != 90*time.Second || cfg.StopTimeout != 10*time.Second {
		t.Errorf("unexpected scalar config: %+v", cfg)
	}
	if len(cfg.IgnoreUserAgents) != 2 || cfg.IgnoreUserAgents[0] != "UptimeRobot" || cfg.IgnoreUserAgents[1] != "Pingdom" {
		t.Errorf("IgnoreUserAgents = %v", cfg.IgnoreUserAgents)
	}
	if len(cfg.IgnorePaths) != 2 || cfg.IgnorePaths[1] != "/ping" {
		t.Errorf("IgnorePaths = %v", cfg.IgnorePaths)
	}
	if len(cfg.IgnoreContainers) != 2 || cfg.IgnoreContainers[0] != "db" || cfg.IgnoreContainers[1] != "cache" {
		t.Errorf("IgnoreContainers = %v", cfg.IgnoreContainers)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("Routes = %v", cfg.Routes)
	}
	if cfg.Routes[0].Prefix != "/api" || cfg.Routes[0].Target().String() != "http://api:9000" {
		t.Errorf("route 0 = %+v", cfg.Routes[0])
	}
	if cfg.ComposeProject != "myproj" || cfg.DockerSocket != "/tmp/docker.sock" {
		t.Errorf("project/socket = %q/%q", cfg.ComposeProject, cfg.DockerSocket)
	}
}

func TestRoutesOnlyWithoutDefaultUpstream(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"SLAYGROUND_ROUTES": "/=http://web:80",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultTarget() != nil {
		t.Error("DefaultTarget should be nil without SLAYGROUND_UPSTREAM")
	}
}

func TestInvalidValues(t *testing.T) {
	cases := map[string]map[string]string{
		"bad duration":     {"SLAYGROUND_UPSTREAM": "http://a", "SLAYGROUND_IDLE_TIMEOUT": "banana"},
		"zero idle":        {"SLAYGROUND_UPSTREAM": "http://a", "SLAYGROUND_IDLE_TIMEOUT": "0s"},
		"bad route format": {"SLAYGROUND_ROUTES": "/api"},
		"bad scheme":       {"SLAYGROUND_UPSTREAM": "ftp://a"},
		"no host":          {"SLAYGROUND_UPSTREAM": "http://"},
		"bad prefix":       {"SLAYGROUND_ROUTES": "api=http://a"},
	}
	for name, env := range cases {
		if _, err := Load(envFrom(env)); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestYAMLFileAndEnvPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
listen: ":9999"
upstream: http://file-app:80
idle_timeout: 10m
ignore_user_agents: [FileAgent]
ignore_paths: [/file-health]
ignore_containers: [db]
routes:
  - prefix: /api
    upstream: http://file-api:80
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(envFrom(map[string]string{
		"SLAYGROUND_CONFIG": path,
		"SLAYGROUND_LISTEN": ":7777", // env wins over file
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":7777" {
		t.Errorf("Listen = %q, want env override :7777", cfg.Listen)
	}
	if cfg.Upstream != "http://file-app:80" || cfg.IdleTimeout != 10*time.Minute {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Prefix != "/api" {
		t.Errorf("Routes = %+v", cfg.Routes)
	}
	if len(cfg.IgnoreUserAgents) != 1 || cfg.IgnoreUserAgents[0] != "FileAgent" {
		t.Errorf("IgnoreUserAgents = %v", cfg.IgnoreUserAgents)
	}
	if len(cfg.IgnoreContainers) != 1 || cfg.IgnoreContainers[0] != "db" {
		t.Errorf("IgnoreContainers = %v", cfg.IgnoreContainers)
	}
}

func TestEnvRoutesReplaceFileRoutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
routes:
  - prefix: /api
    upstream: http://file-api:80
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(envFrom(map[string]string{
		"SLAYGROUND_CONFIG": path,
		"SLAYGROUND_ROUTES": "/env=http://env:80",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Prefix != "/env" {
		t.Errorf("Routes = %+v, want env routes only", cfg.Routes)
	}
}

func TestMissingConfigFile(t *testing.T) {
	_, err := Load(envFrom(map[string]string{
		"SLAYGROUND_CONFIG":   "/nonexistent/config.yaml",
		"SLAYGROUND_UPSTREAM": "http://a",
	}))
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
}
