package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jclement/slayground/internal/config"
)

type fakeCtrl struct {
	mu          sync.Mutex
	suspends    int
	resumes     int
	suspendErr  error
	resumeDelay time.Duration
}

func (f *fakeCtrl) Suspend(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspends++
	return f.suspendErr
}

func (f *fakeCtrl) Resume(context.Context) error {
	f.mu.Lock()
	f.resumes++
	delay := f.resumeDelay
	f.mu.Unlock()
	time.Sleep(delay)
	return nil
}

func (f *fakeCtrl) counts() (suspends, resumes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suspends, f.resumes
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newServer(t *testing.T, env map[string]string, ctrl StackController) *Server {
	t.Helper()
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, ctrl, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runServer starts the idle/resume loop for the test's duration.
func runServer(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Run(ctx)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPlainProxyPassthrough(t *testing.T) {
	var gotHost, gotForwardedFor, gotCustom, gotMethod, gotQuery, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		gotCustom = r.Header.Get("X-Custom-Header")
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("X-Upstream-Header", "yes")
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "hello from upstream")
	}))
	defer upstream.Close()

	s := newServer(t, map[string]string{"SLAYGROUND_UPSTREAM": upstream.URL}, nil)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/some/path?a=1&b=2", strings.NewReader("payload"))
	req.Host = "public.example.com"
	req.Header.Set("X-Custom-Header", "custom-value")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot || string(body) != "hello from upstream" {
		t.Errorf("response = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Upstream-Header") != "yes" {
		t.Error("upstream response header not passed through")
	}
	if gotHost != "public.example.com" {
		t.Errorf("upstream saw Host %q, want original host", gotHost)
	}
	if gotForwardedFor == "" {
		t.Error("X-Forwarded-For not set")
	}
	if gotCustom != "custom-value" || gotMethod != http.MethodPost || gotQuery != "a=1&b=2" || gotBody != "payload" {
		t.Errorf("request not passed through: custom=%q method=%q query=%q body=%q",
			gotCustom, gotMethod, gotQuery, gotBody)
	}
}

func TestRouting(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "api")
	}))
	defer api.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "web")
	}))
	defer web.Close()

	s := newServer(t, map[string]string{
		"SLAYGROUND_UPSTREAM": web.URL,
		"SLAYGROUND_ROUTES":   "/api=" + api.URL,
	}, nil)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	cases := map[string]string{
		"/":         "web",
		"/api":      "api",
		"/api/v1/x": "api",
		"/apiary":   "web", // prefix must match on segment boundaries
		"/other":    "web",
	}
	for path, want := range cases {
		resp, err := http.Get(proxySrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != want {
			t.Errorf("GET %s -> %q, want %q", path, body, want)
		}
	}
}

func TestNoRouteNoFallbackIs502(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "api")
	}))
	defer api.Close()

	s := newServer(t, map[string]string{"SLAYGROUND_ROUTES": "/api=" + api.URL}, nil)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMatchPrefix(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/anything", "/", true},
		{"/api", "/api", true},
		{"/api/", "/api", true},
		{"/api/v1", "/api", true},
		{"/apiary", "/api", false},
		{"/", "/api", false},
		{"/api/v1", "/api/", true},
		{"/health", "/health", true},
		{"/healthz", "/health", false},
	}
	for _, c := range cases {
		if got := matchPrefix(c.path, c.prefix); got != c.want {
			t.Errorf("matchPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

func TestBootResumesThenServes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "up")
	}))
	defer upstream.Close()

	ctrl := &fakeCtrl{resumeDelay: 100 * time.Millisecond}
	s := newServer(t, map[string]string{
		"SLAYGROUND_UPSTREAM":     upstream.URL,
		"SLAYGROUND_IDLE_TIMEOUT": "1h",
	}, ctrl)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	// Before Run, the server reports itself as starting and serves the wait
	// page rather than proxying to a stack that may be down.
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(page), "Waking things up") {
		t.Errorf("pre-resume response = %d %q", resp.StatusCode, page)
	}

	runServer(t, s)
	waitFor(t, "boot resume", func() bool { return s.State() == "up" })

	resp, err = http.Get(proxySrv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "up" {
		t.Errorf("post-resume body = %q", body)
	}
}

func TestSuspendsWhenIdleAndWakesOnRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "up")
	}))
	defer upstream.Close()

	ctrl := &fakeCtrl{}
	s := newServer(t, map[string]string{
		"SLAYGROUND_UPSTREAM":     upstream.URL,
		"SLAYGROUND_IDLE_TIMEOUT": "80ms",
	}, ctrl)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()
	runServer(t, s)

	waitFor(t, "idle suspend", func() bool { return s.State() == "suspended" })
	if suspends, _ := ctrl.counts(); suspends != 1 {
		t.Errorf("suspends = %d, want 1", suspends)
	}

	// A browser request while suspended gets the wait page and wakes the stack.
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/app", nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(page), "Waking things up") {
		t.Errorf("wake response = %d %q", resp.StatusCode, page)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("Retry-After not set on wait page")
	}

	waitFor(t, "wake resume", func() bool { return s.State() == "up" })
	if _, resumes := ctrl.counts(); resumes != 2 { // boot + wake
		t.Errorf("resumes = %d, want 2", resumes)
	}

	// Traffic flows again after the resume.
	resp, err = http.Get(proxySrv.URL + "/app")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "up" {
		t.Errorf("post-wake body = %q", body)
	}
}

func TestIgnoredRequestsDoNotKeepAliveOrWake(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "up")
	}))
	defer upstream.Close()

	ctrl := &fakeCtrl{}
	s := newServer(t, map[string]string{
		"SLAYGROUND_UPSTREAM":           upstream.URL,
		"SLAYGROUND_IDLE_TIMEOUT":       "100ms",
		"SLAYGROUND_IGNORE_PATHS":       "/health",
		"SLAYGROUND_IGNORE_USER_AGENTS": "UptimeRobot",
	}, ctrl)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()
	runServer(t, s)
	waitFor(t, "boot resume", func() bool { return s.State() == "up" })

	// While up, ignored requests are still proxied.
	resp, err := http.Get(proxySrv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "up" {
		t.Errorf("/health while up = %d %q, want proxied 200", resp.StatusCode, body)
	}

	// Hammer ignored endpoints; the stack must still suspend.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if s.State() == "suspended" {
				return
			}
			r, err := http.Get(proxySrv.URL + "/health")
			if err == nil {
				r.Body.Close()
			}
			req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/app", nil)
			req.Header.Set("User-Agent", "UptimeRobot/2.0 (http://uptimerobot.com)")
			if r, err := http.DefaultClient.Do(req); err == nil {
				r.Body.Close()
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	waitFor(t, "suspend despite ignored traffic", func() bool { return s.State() == "suspended" })
	<-done

	// Ignored requests while suspended get a 503 and do not wake the stack.
	resp, err = http.Get(proxySrv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/health while suspended = %d, want 503", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	if s.State() != "suspended" {
		t.Errorf("state = %s, want still suspended", s.State())
	}
	if _, resumes := ctrl.counts(); resumes != 1 { // boot only
		t.Errorf("resumes = %d, want 1", resumes)
	}
}

func TestWaitPageContentNegotiation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	// Never call Run: the server stays in "starting" forever.
	ctrl := &fakeCtrl{}
	s := newServer(t, map[string]string{"SLAYGROUND_UPSTREAM": upstream.URL}, ctrl)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(body), "<html") {
		t.Errorf("browser request got %q %q", resp.Header.Get("Content-Type"), body)
	}

	req, _ = http.NewRequest(http.MethodGet, proxySrv.URL+"/", nil)
	req.Header.Set("Accept", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "<html") {
		t.Errorf("API request got HTML: %q", body)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("API request = %d, Retry-After = %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestSuspendFailureLeavesStackUp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "up")
	}))
	defer upstream.Close()

	ctrl := &fakeCtrl{suspendErr: fmt.Errorf("docker exploded")}
	s := newServer(t, map[string]string{
		"SLAYGROUND_UPSTREAM":     upstream.URL,
		"SLAYGROUND_IDLE_TIMEOUT": "60ms",
	}, ctrl)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()
	runServer(t, s)

	// Suspends keep being attempted (and failing), but traffic still flows.
	waitFor(t, "suspend retries", func() bool {
		suspends, _ := ctrl.counts()
		return suspends >= 2
	})
	if s.State() != "up" && s.State() != "suspending" {
		t.Errorf("state = %s, want up/suspending", s.State())
	}
	resp, err := http.Get(proxySrv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "up" {
		t.Errorf("body = %q, want proxied response", body)
	}
}

func TestWebSocketPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf.Flush()
		line, err := buf.ReadString('\n')
		if err != nil {
			return
		}
		buf.WriteString("echo:" + line)
		buf.Flush()
	}))
	defer upstream.Close()

	s := newServer(t, map[string]string{"SLAYGROUND_UPSTREAM": upstream.URL}, nil)
	proxySrv := httptest.NewServer(s)
	defer proxySrv.Close()

	u, _ := url.Parse(proxySrv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101", status)
	}
	// Skip response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if echo != "echo:hello\n" {
		t.Errorf("echo = %q", echo)
	}
}
