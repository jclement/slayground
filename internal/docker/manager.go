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
// (docker compose run ...), or containers labeled slayground.exclude=true.
type Manager struct {
	Client         *Client
	Project        string
	SelfID         string
	StopTimeout    time.Duration
	StartupTimeout time.Duration
	Log            *slog.Logger
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

// Resume starts every stopped managed container in the project and waits,
// up to StartupTimeout, for the stack to become ready: each managed
// container must be running (and healthy, if it defines a healthcheck) or
// have exited cleanly (one-shot init containers).
func (m *Manager) Resume(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, m.StartupTimeout)
	defer cancel()

	containers, err := m.Client.ListProject(ctx, m.Project)
	if err != nil {
		return fmt.Errorf("listing project containers: %w", err)
	}
	var errs []error
	for _, c := range containers {
		if !m.managed(c) || c.State == "running" {
			continue
		}
		m.Log.Info("starting container", "container", c.Name)
		if err := m.Client.Start(ctx, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("starting %s: %w", c.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	lastLog := time.Now()
	for {
		waiting, err := m.notReady(ctx)
		if err != nil {
			return err
		}
		if waiting == "" {
			return nil
		}
		if time.Since(lastLog) >= 5*time.Second {
			m.Log.Info("waiting for containers to become ready", "waiting_on", waiting)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stack not ready before startup timeout; still waiting on %s", waiting)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// notReady returns a description of the managed containers that are not yet
// ready, or "" when the stack is ready.
func (m *Manager) notReady(ctx context.Context) (string, error) {
	containers, err := m.Client.ListProject(ctx, m.Project)
	if err != nil {
		return "", fmt.Errorf("listing project containers: %w", err)
	}
	var waiting []string
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
		case ins.State.ExitCode == 0:
			// exited cleanly: a completed one-shot container
		default:
			waiting = append(waiting, fmt.Sprintf("%s (exited: %d)", c.Name, ins.State.ExitCode))
		}
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
	return true
}
