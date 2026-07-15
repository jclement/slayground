package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Manager suspends and resumes the sibling containers of a Compose project.
// It never touches slayground's own container, Compose one-off containers
// (docker compose run ...), containers labeled slayground.exclude=true, or
// containers listed in IgnoreContainers.
type Manager struct {
	Client         *Client
	Project        string
	SelfID         string
	StopTimeout    time.Duration
	StartupTimeout time.Duration
	// IgnoreContainers holds Compose service names or full container names
	// to leave alone (case-insensitive).
	IgnoreContainers []string
	Log              *slog.Logger
}

// Suspend stops every running managed container in the project.
func (m *Manager) Suspend(ctx context.Context) error {
	containers, err := m.Client.ListProject(ctx, m.Project)
	if err != nil {
		return fmt.Errorf("listing project containers: %w", err)
	}
	var errs []error
	stopped := 0
	for _, c := range containers {
		if !m.managed(c) || c.State != "running" {
			continue
		}
		m.Log.Info("stopping container", "container", c.Name)
		if err := m.Client.Stop(ctx, c.ID, m.StopTimeout); err != nil {
			errs = append(errs, fmt.Errorf("stopping %s: %w", c.Name, err))
			continue
		}
		stopped++
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	m.Log.Info("containers stopped", "project", m.Project, "count", stopped)
	return nil
}

// maxStartAttempts bounds how many times Resume will try to start any one
// container before giving up on it (it keeps waiting until the timeout in
// case the container comes up anyway).
const maxStartAttempts = 3

// Resume starts the project's stopped managed containers and waits, up to
// StartupTimeout, for the stack to become ready: each managed container must
// be running (and healthy, if it defines a healthcheck) or have exited
// cleanly after being started (one-shot init containers).
//
// Docker can report a just-stopped container as still "running" for a brief
// moment after the stop call returns, so a single start pass can silently
// miss containers. Instead, the polling loop itself (re)starts anything it
// finds not running — a bounded number of times per container — and
// readiness must be observed on two consecutive polls before Resume returns.
func (m *Manager) Resume(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, m.StartupTimeout)
	defer cancel()

	attempts := map[string]int{}
	lastLog := time.Now()
	confirmed := false
	for {
		waiting, err := m.startAndCheck(ctx, attempts)
		if err != nil {
			return err
		}
		if waiting == "" {
			if confirmed {
				return nil
			}
			confirmed = true
		} else {
			confirmed = false
			if time.Since(lastLog) >= 5*time.Second {
				m.Log.Info("waiting for containers to become ready", "waiting_on", waiting)
				lastLog = time.Now()
			}
		}
		delay := 500 * time.Millisecond
		if confirmed {
			delay = 250 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			if waiting == "" {
				return nil
			}
			return fmt.Errorf("stack not ready before startup timeout; still waiting on %s", waiting)
		case <-time.After(delay):
		}
	}
}

// startAndCheck inspects every managed container, starting any that are not
// running (up to maxStartAttempts each), and returns a description of the
// containers that are not yet ready — "" when the whole stack is ready.
func (m *Manager) startAndCheck(ctx context.Context, attempts map[string]int) (string, error) {
	containers, err := m.Client.ListProject(ctx, m.Project)
	if err != nil {
		return "", fmt.Errorf("listing project containers: %w", err)
	}
	var waiting []string
	var errs []error
	for _, c := range containers {
		if !m.managed(c) {
			continue
		}
		ins, err := m.Client.Inspect(ctx, c.ID)
		if err != nil {
			return "", fmt.Errorf("inspecting %s: %w", c.Name, err)
		}
		switch {
		case ins.State.Running && ins.State.Health == nil:
			// running, no healthcheck: ready
		case ins.State.Running && ins.State.Health.Status == "healthy":
			// running and healthy: ready
		case ins.State.Running:
			waiting = append(waiting, fmt.Sprintf("%s (health: %s)", c.Name, ins.State.Health.Status))
		case ins.State.ExitCode == 0 && attempts[c.ID] > 0:
			// exited cleanly after we started it: a completed one-shot
		case attempts[c.ID] >= maxStartAttempts:
			waiting = append(waiting, fmt.Sprintf("%s (exited: %d; start attempts exhausted)", c.Name, ins.State.ExitCode))
		default:
			attempts[c.ID]++
			m.Log.Info("starting container", "container", c.Name, "attempt", attempts[c.ID])
			if err := m.Client.Start(ctx, c.ID); err != nil {
				errs = append(errs, fmt.Errorf("starting %s: %w", c.Name, err))
			}
			waiting = append(waiting, fmt.Sprintf("%s (starting)", c.Name))
		}
	}
	if len(errs) > 0 {
		return "", errors.Join(errs...)
	}
	return strings.Join(waiting, ", "), nil
}

// managed reports whether slayground should stop/start this container.
func (m *Manager) managed(c Container) bool {
	if c.ID == m.SelfID {
		return false
	}
	if strings.EqualFold(c.Labels[LabelOneOff], "true") {
		return false
	}
	switch strings.ToLower(c.Labels[LabelExclude]) {
	case "true", "1", "yes":
		return false
	}
	for _, name := range m.IgnoreContainers {
		if strings.EqualFold(name, c.Labels[LabelService]) || strings.EqualFold(name, c.Name) {
			return false
		}
	}
	return true
}
