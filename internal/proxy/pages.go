package proxy

import (
	"fmt"
	"net/http"
	"strings"
)

// serveWaitPage answers a request that arrived while the stack is waking:
// browsers get an auto-refreshing page, everything else gets a plain 503
// with Retry-After so well-behaved clients retry on their own.
func serveWaitPage(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, http.StatusServiceUnavailable,
		"slayground: stack is starting, retry shortly",
		"Waking things up&hellip;",
		"This service was asleep to save resources. It&rsquo;s starting now &mdash; this page will refresh automatically.")
}

// serveUpstreamErrorPage answers a request whose upstream could not be
// reached: same treatment, but as a 502 with an honest message.
func serveUpstreamErrorPage(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, http.StatusBadGateway,
		"slayground: upstream unreachable, retry shortly",
		"Almost there&hellip;",
		"The service isn&rsquo;t answering yet &mdash; it may still be starting up. This page will keep retrying automatically.")
}

func servePage(w http.ResponseWriter, r *http.Request, status int, plainText, heading, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "3")
	if !wantsHTML(r) {
		http.Error(w, plainText, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, pageTemplate, heading, heading, message)
}

// wantsHTML reports whether the client explicitly accepts HTML. A bare */*
// (curl, most API clients) intentionally does not count as a browser.
func wantsHTML(r *http.Request) bool {
	for _, accept := range r.Header.Values("Accept") {
		for _, part := range strings.Split(accept, ",") {
			mediaType, _, _ := strings.Cut(part, ";")
			switch strings.TrimSpace(mediaType) {
			case "text/html", "application/xhtml+xml":
				return true
			}
		}
	}
	return false
}

// pageTemplate takes three %s values: title, heading, and message. All of
// them are trusted compile-time constants, never user input.
const pageTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<noscript><meta http-equiv="refresh" content="3"></noscript>
<style>
  :root { color-scheme: light dark; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    font-family: system-ui, -apple-system, "Segoe UI", sans-serif;
    background: light-dark(#f6f7f9, #16181d); color: light-dark(#1f2328, #e6e8eb);
  }
  .card { text-align: center; padding: 3rem 2.5rem; }
  .spinner {
    width: 44px; height: 44px; margin: 0 auto 1.5rem;
    border: 4px solid light-dark(#d4d9e0, #313640); border-top-color: #4f7df9;
    border-radius: 50%%; animation: spin 0.9s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  h1 { font-size: 1.3rem; font-weight: 600; margin: 0 0 0.5rem; }
  p { margin: 0; color: light-dark(#59636e, #9aa2ad); font-size: 0.95rem; }
</style>
</head>
<body>
<div class="card">
  <div class="spinner" role="status" aria-label="Loading"></div>
  <h1>%s</h1>
  <p>%s</p>
</div>
<script>setTimeout(function () { location.reload(); }, 3000);</script>
</body>
</html>
`
