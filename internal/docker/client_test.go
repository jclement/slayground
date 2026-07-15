package docker

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func projectLabels(project string) map[string]string {
	return map[string]string{LabelProject: project}
}

func TestPing(t *testing.T) {
	client := startFakeDaemon(t, newFakeDaemon())
	if err := client.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPingUnreachable(t *testing.T) {
	client := NewClient("/nonexistent/docker.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx); err == nil {
		t.Fatal("expected error for missing socket")
	}
}

func TestListProject(t *testing.T) {
	daemon := newFakeDaemon(
		&fakeContainer{id: "aaa", name: "proj-web-1", state: "running", labels: projectLabels("proj")},
		&fakeContainer{id: "bbb", name: "proj-db-1", state: "exited", labels: projectLabels("proj")},
		&fakeContainer{id: "ccc", name: "other-app-1", state: "running", labels: projectLabels("other")},
	)
	client := startFakeDaemon(t, daemon)

	containers, err := client.ListProject(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 {
		t.Fatalf("got %d containers, want 2: %+v", len(containers), containers)
	}
	for _, c := range containers {
		if !strings.HasPrefix(c.Name, "proj-") {
			t.Errorf("unexpected container %+v", c)
		}
		if strings.HasPrefix(c.Name, "/") {
			t.Errorf("name %q not trimmed", c.Name)
		}
	}
}

func TestStopAndStart(t *testing.T) {
	c := &fakeContainer{id: "aaa", name: "proj-web-1", state: "running", labels: projectLabels("proj")}
	daemon := newFakeDaemon(c)
	client := startFakeDaemon(t, daemon)
	ctx := context.Background()

	if err := client.Stop(ctx, "aaa", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// Stopping again returns 304, which must not be an error.
	if err := client.Stop(ctx, "aaa", 10*time.Second); err != nil {
		t.Fatalf("stop of stopped container: %v", err)
	}
	if err := client.Start(ctx, "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx, "aaa"); err != nil {
		t.Fatalf("start of running container: %v", err)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if c.stops != 1 || c.starts != 1 {
		t.Errorf("stops=%d starts=%d, want 1/1", c.stops, c.starts)
	}
}

func TestErrorIncludesDaemonMessage(t *testing.T) {
	client := startFakeDaemon(t, newFakeDaemon())
	_, err := client.Inspect(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("err = %v, want daemon message", err)
	}
}

func TestDiscoverSelf(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	daemon := newFakeDaemon(
		&fakeContainer{id: host, name: "proj-slayground-1", state: "running", labels: projectLabels("proj")},
	)
	client := startFakeDaemon(t, daemon)

	project, id, err := client.DiscoverSelf(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if project != "proj" || id != host {
		t.Errorf("got project=%q id=%q", project, id)
	}
}

func TestDiscoverSelfNotInCompose(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	daemon := newFakeDaemon(
		&fakeContainer{id: host, name: "lonely", state: "running", labels: map[string]string{}},
	)
	client := startFakeDaemon(t, daemon)

	if _, _, err := client.DiscoverSelf(context.Background()); err == nil {
		t.Fatal("expected error for container without compose label")
	}
}
