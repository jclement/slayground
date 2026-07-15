package docker

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeContainer is a container in the fake daemon.
type fakeContainer struct {
	id     string
	name   string
	state  string // "running" or "exited"
	labels map[string]string

	exitCode int
	// hasHealth marks the container as having a healthcheck; after a start,
	// health reports "starting" for healthyAfterInspects inspects, then
	// "healthy" (or "unhealthy" forever if stuckUnhealthy).
	hasHealth            bool
	healthyAfterInspects int
	stuckUnhealthy       bool
	// staysExited simulates a one-shot container: start leaves it exited.
	staysExited bool
	// staleRunningInspects makes the next N inspects report Running=true
	// regardless of actual state, simulating Docker's brief stale-state
	// window right after a stop returns.
	staleRunningInspects int

	stops, starts, inspects int
}

// fakeDaemon implements the subset of the Docker Engine API the client uses.
type fakeDaemon struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
}

func newFakeDaemon(cs ...*fakeContainer) *fakeDaemon {
	d := &fakeDaemon{containers: map[string]*fakeContainer{}}
	for _, c := range cs {
		d.containers[c.id] = c
	}
	return d
}

func (d *fakeDaemon) get(ref string) *fakeContainer {
	if c, ok := d.containers[ref]; ok {
		return c
	}
	for _, c := range d.containers {
		if c.name == ref {
			return c
		}
	}
	return nil
}

func (d *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "OK")
	})

	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		var filters struct {
			Label []string `json:"label"`
		}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		var out []map[string]any
		for _, c := range d.containers {
			if !matchesLabels(c, filters.Label) {
				continue
			}
			out = append(out, map[string]any{
				"Id":     c.id,
				"Names":  []string{"/" + c.name},
				"State":  c.state,
				"Labels": c.labels,
			})
		}
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		c := d.get(r.PathValue("id"))
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + r.PathValue("id")})
			return
		}
		c.inspects++
		running := c.state == "running"
		if c.staleRunningInspects > 0 {
			c.staleRunningInspects--
			running = true
		}
		state := map[string]any{
			"Running":  running,
			"ExitCode": c.exitCode,
		}
		if c.hasHealth {
			status := "healthy"
			if c.stuckUnhealthy {
				status = "unhealthy"
			} else if c.healthyAfterInspects > 0 {
				c.healthyAfterInspects--
				status = "starting"
			}
			state["Health"] = map[string]any{"Status": status}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Id":     c.id,
			"Name":   "/" + c.name,
			"State":  state,
			"Config": map[string]any{"Labels": c.labels},
		})
	})

	mux.HandleFunc("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		c := d.get(r.PathValue("id"))
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container"})
			return
		}
		if c.state != "running" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.state = "exited"
		c.stops++
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		c := d.get(r.PathValue("id"))
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "No such container"})
			return
		}
		if c.state == "running" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		c.starts++
		if !c.staysExited {
			c.state = "running"
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func matchesLabels(c *fakeContainer, labelFilters []string) bool {
	for _, f := range labelFilters {
		k, v, hasValue := strings.Cut(f, "=")
		got, ok := c.labels[k]
		if !ok || (hasValue && got != v) {
			return false
		}
	}
	return true
}

// startFakeDaemon serves the fake daemon on a unix socket and returns a
// Client pointed at it.
func startFakeDaemon(t *testing.T, d *fakeDaemon) *Client {
	t.Helper()
	// os.MkdirTemp instead of t.TempDir: unix socket paths have a ~104 byte
	// limit on macOS and t.TempDir paths can exceed it.
	dir, err := os.MkdirTemp("", "slayground")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d.handler()}
	go srv.Serve(l)
	t.Cleanup(func() {
		srv.Close()
		os.RemoveAll(dir)
	})
	return NewClient(sock)
}
