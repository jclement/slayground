// Package proxy implements slayground's HTTP reverse proxy and the
// idle-suspend / wake-on-request state machine around it.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jclement/slayground/internal/config"
)

// StackController stops and starts the rest of the Compose stack.
type StackController interface {
	// Suspend stops the stack's other containers.
	Suspend(ctx context.Context) error
	// Resume starts the stack's other containers and blocks until they are
	// ready (or its startup timeout elapses).
	Resume(ctx context.Context) error
}

type state int

const (
	stateUp state = iota
	stateSuspending
	stateSuspended
	stateStarting
)

func (s state) String() string {
	switch s {
	case stateUp:
		return "up"
	case stateSuspending:
		return "suspending"
	case stateSuspended:
		return "suspended"
	case stateStarting:
		return "starting"
	}
	return "unknown"
}

type route struct {
	prefix string
	proxy  *httputil.ReverseProxy
}

// Server is the slayground HTTP handler: it routes requests to upstreams
// while the stack is up, and serves a wait page (waking the stack) while it
// is not.
type Server struct {
	routes           []route // sorted by prefix length, longest first
	fallback         *httputil.ReverseProxy
	ignoreUserAgents []string // lowercased substrings
	ignorePaths      []string
	idleTimeout      time.Duration
	ctrl             StackController
	log              *slog.Logger

	mu           sync.Mutex
	st           state
	lastActivity time.Time
}

// New builds a Server from cfg. ctrl may be nil, in which case the server is
// a plain always-on proxy (no suspend/resume).
func New(cfg *config.Config, ctrl StackController, log *slog.Logger) (*Server, error) {
	s := &Server{
		idleTimeout: cfg.IdleTimeout,
		ctrl:        ctrl,
		log:         log,
		ignorePaths: cfg.IgnorePaths,
	}
	for _, ua := range cfg.IgnoreUserAgents {
		s.ignoreUserAgents = append(s.ignoreUserAgents, strings.ToLower(ua))
	}
	if t := cfg.DefaultTarget(); t != nil {
		s.fallback = s.newReverseProxy(t)
	}
	for _, r := range cfg.Routes {
		s.routes = append(s.routes, route{prefix: r.Prefix, proxy: s.newReverseProxy(r.Target())})
	}
	sort.SliceStable(s.routes, func(i, j int) bool {
		return len(s.routes[i].prefix) > len(s.routes[j].prefix)
	})

	s.lastActivity = time.Now()
	if ctrl != nil {
		// Start suspended-side: Run kicks off a resume so the stack is
		// known-good (started and healthy) before we forward anything.
		s.st = stateStarting
	} else {
		s.st = stateUp
	}
	return s, nil
}

// Run performs the initial stack resume and then watches for idleness until
// ctx is cancelled. It must be started once, alongside serving.
func (s *Server) Run(ctx context.Context) {
	if s.ctrl == nil {
		return
	}
	go s.resume("startup")

	tick := s.idleTimeout / 10
	tick = max(tick, 25*time.Millisecond)
	tick = min(tick, 10*time.Second)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maybeSuspend(ctx)
		}
	}
}

// State returns the current state name (for logging and tests).
func (s *Server) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.String()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ignored := s.isIgnored(r)

	s.mu.Lock()
	if !ignored {
		s.lastActivity = time.Now()
	}
	st := s.st
	wake := false
	if st == stateSuspended && !ignored {
		s.st = stateStarting
		st = stateStarting
		wake = true
	}
	s.mu.Unlock()

	if wake {
		s.log.Info("request while suspended; waking stack",
			"path", r.URL.Path, "user_agent", r.UserAgent())
		go s.resume("incoming request")
	}

	if st == stateUp {
		s.route(r.URL.Path).ServeHTTP(w, r)
		return
	}

	w.Header().Set("X-Slayground-State", st.String())
	w.Header().Set("Cache-Control", "no-store")
	if ignored {
		// Health checkers and ignored paths must not wake the stack; tell
		// them plainly that it is asleep.
		http.Error(w, "slayground: stack is "+st.String(), http.StatusServiceUnavailable)
		return
	}
	serveWaitPage(w, r)
}

// route returns the handler for a request path: the longest matching route
// prefix, then the default upstream, then a 502.
func (s *Server) route(path string) http.Handler {
	for _, rt := range s.routes {
		if matchPrefix(path, rt.prefix) {
			return rt.proxy
		}
	}
	if s.fallback != nil {
		return s.fallback
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "slayground: no route for this path and no default upstream configured", http.StatusBadGateway)
	})
}

// matchPrefix reports whether path falls under prefix on path-segment
// boundaries: /api matches /api and /api/v1 but not /apiary.
func matchPrefix(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// isIgnored reports whether the request should neither count as activity nor
// wake a suspended stack.
func (s *Server) isIgnored(r *http.Request) bool {
	for _, p := range s.ignorePaths {
		if matchPrefix(r.URL.Path, p) {
			return true
		}
	}
	if len(s.ignoreUserAgents) > 0 {
		ua := strings.ToLower(r.UserAgent())
		for _, frag := range s.ignoreUserAgents {
			if strings.Contains(ua, frag) {
				return true
			}
		}
	}
	return false
}

func (s *Server) newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Preserve the original Host so upstream apps behind
			// Cloudflare/Tor see the domain the client asked for.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		// Flush immediately: keeps SSE and other streaming responses live.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				// The client hung up before the upstream answered (common
				// for health checkers with short timeouts) — not an
				// upstream problem, so keep it out of the warn stream.
				s.log.Debug("client canceled request", "path", r.URL.Path)
			} else {
				s.log.Warn("upstream error", "path", r.URL.Path, "upstream", target.String(), "error", err)
			}
			serveUpstreamErrorPage(w, r)
		},
	}
}

// maybeSuspend stops the stack if it has been idle past the timeout.
func (s *Server) maybeSuspend(ctx context.Context) {
	s.mu.Lock()
	idle := time.Since(s.lastActivity)
	if s.st != stateUp || idle < s.idleTimeout {
		s.mu.Unlock()
		return
	}
	s.st = stateSuspending
	decision := time.Now()
	s.mu.Unlock()

	s.log.Info("stack idle; suspending", "idle", idle.Round(time.Second))
	err := s.ctrl.Suspend(ctx)

	s.mu.Lock()
	if err != nil {
		// Leave the stack up; the next tick retries.
		s.st = stateUp
		s.mu.Unlock()
		s.log.Error("suspend failed", "error", err)
		return
	}
	if s.lastActivity.After(decision) {
		// A request arrived while we were stopping containers.
		s.st = stateStarting
		s.mu.Unlock()
		s.log.Info("activity during suspend; resuming immediately")
		go s.resume("activity during suspend")
		return
	}
	s.st = stateSuspended
	s.mu.Unlock()
	s.log.Info("stack suspended")
}

// resume brings the stack back up and marks the proxy live. On error the
// proxy still goes live: forwarding (and surfacing 502s) beats trapping every
// client on the wait page forever.
func (s *Server) resume(reason string) {
	start := time.Now()
	s.log.Info("resuming stack", "reason", reason)
	err := s.ctrl.Resume(context.Background())

	s.mu.Lock()
	s.st = stateUp
	s.lastActivity = time.Now()
	s.mu.Unlock()

	if err != nil {
		s.log.Warn("stack resumed but may not be fully ready", "error", err, "took", time.Since(start).Round(time.Millisecond))
		return
	}
	s.log.Info("stack resumed", "took", time.Since(start).Round(time.Millisecond))
}
