package docker

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testManager(client *Client) *Manager {
	return &Manager{
		Client:         client,
		Project:        "proj",
		SelfID:         "self",
		StopTimeout:    5 * time.Second,
		StartupTimeout: 5 * time.Second,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestSuspendStopsOnlyManagedRunningContainers(t *testing.T) {
	web := &fakeContainer{id: "web", name: "proj-web-1", state: "running", labels: projectLabels("proj")}
	db := &fakeContainer{id: "db", name: "proj-db-1", state: "running", labels: projectLabels("proj")}
	self := &fakeContainer{id: "self", name: "proj-slayground-1", state: "running", labels: projectLabels("proj")}
	oneoff := &fakeContainer{id: "oneoff", name: "proj-run-1", state: "running", labels: map[string]string{
		LabelProject: "proj", LabelOneOff: "True",
	}}
	excluded := &fakeContainer{id: "keep", name: "proj-keep-1", state: "running", labels: map[string]string{
		LabelProject: "proj", LabelExclude: "true",
	}}
	stopped := &fakeContainer{id: "old", name: "proj-old-1", state: "exited", labels: projectLabels("proj")}
	daemon := newFakeDaemon(web, db, self, oneoff, excluded, stopped)
	m := testManager(startFakeDaemon(t, daemon))

	if err := m.Suspend(context.Background()); err != nil {
		t.Fatal(err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if web.stops != 1 || db.stops != 1 {
		t.Errorf("web/db stops = %d/%d, want 1/1", web.stops, db.stops)
	}
	for _, c := range []*fakeContainer{self, oneoff, excluded, stopped} {
		if c.stops != 0 {
			t.Errorf("%s was stopped but should not be", c.name)
		}
	}
}

func TestResumeStartsAndWaitsForHealth(t *testing.T) {
	web := &fakeContainer{
		id: "web", name: "proj-web-1", state: "exited", labels: projectLabels("proj"),
		hasHealth: true, healthyAfterInspects: 2,
	}
	db := &fakeContainer{id: "db", name: "proj-db-1", state: "exited", labels: projectLabels("proj")}
	self := &fakeContainer{id: "self", name: "proj-slayground-1", state: "running", labels: projectLabels("proj")}
	daemon := newFakeDaemon(web, db, self)
	m := testManager(startFakeDaemon(t, daemon))

	if err := m.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if web.starts != 1 || db.starts != 1 {
		t.Errorf("web/db starts = %d/%d, want 1/1", web.starts, db.starts)
	}
	if self.starts != 0 {
		t.Error("self was started")
	}
	if web.state != "running" || db.state != "running" {
		t.Errorf("states: web=%s db=%s", web.state, db.state)
	}
	// The health wait must have inspected web more than once (it reported
	// "starting" for the first two inspects).
	if web.inspects < 3 {
		t.Errorf("web inspected %d times, want >= 3", web.inspects)
	}
}

func TestResumeTimesOutOnUnhealthyContainer(t *testing.T) {
	web := &fakeContainer{
		id: "web", name: "proj-web-1", state: "exited", labels: projectLabels("proj"),
		hasHealth: true, stuckUnhealthy: true,
	}
	daemon := newFakeDaemon(web)
	m := testManager(startFakeDaemon(t, daemon))
	m.StartupTimeout = 700 * time.Millisecond

	err := m.Resume(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "proj-web-1") {
		t.Errorf("error should name the unhealthy container: %v", err)
	}
}

func TestResumeAcceptsCompletedOneShotContainers(t *testing.T) {
	init := &fakeContainer{
		id: "init", name: "proj-init-1", state: "exited", exitCode: 0,
		labels: projectLabels("proj"), staysExited: true,
	}
	web := &fakeContainer{id: "web", name: "proj-web-1", state: "exited", labels: projectLabels("proj")}
	daemon := newFakeDaemon(init, web)
	m := testManager(startFakeDaemon(t, daemon))

	if err := m.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestResumeReportsCrashedContainer(t *testing.T) {
	web := &fakeContainer{
		id: "web", name: "proj-web-1", state: "exited", exitCode: 1,
		labels: projectLabels("proj"), staysExited: true,
	}
	daemon := newFakeDaemon(web)
	m := testManager(startFakeDaemon(t, daemon))
	m.StartupTimeout = 700 * time.Millisecond

	err := m.Resume(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited: 1") {
		t.Fatalf("err = %v, want crashed-container report", err)
	}
}
