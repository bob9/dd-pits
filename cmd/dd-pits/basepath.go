package main

// Serving dd-pits under a sub-path, e.g. https://rh.example.com/pits/.
//
// dd-pits' pages link to their own addresses from the root (/live, /setup,
// /assets/…). That is right when people open dd-pits on its own port, which is
// how a venue uses it. Behind a reverse proxy that shares a hostname with
// something else — a RotorHazard timer published on the internet, whose own
// pages also live at the root — dd-pits needs a prefix instead.
//
// The proxy says so per request with the standard X-Forwarded-Prefix header:
//
//	location /pits/ {
//	    proxy_pass http://127.0.0.1:8088;
//	    proxy_set_header X-Forwarded-Prefix /pits;
//	}
//
// For those requests only, the prefix is stripped from the path on the way in
// and added to dd-pits' own links on the way out (pages, scripts, styles and
// redirects). Requests straight to the port are untouched, so the same
// dd-pits serves both the venue wifi and the website.

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var prefixRE = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// forwardedPrefix is the sub-path a proxy serves this request under, or "".
func forwardedPrefix(r *http.Request) string {
	p := strings.TrimRight(strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix")), "/")
	if !prefixRE.MatchString(p) {
		return ""
	}
	return p
}

// dd-pits' top-level addresses: a quoted string starting with one of these is
// a link to dd-pits, wherever it appears in a script.
const ddRoutes = `assets|booth\.wav|booth|calls|commentary|dronedash|fpvtrackside|goggles|healthz|live|` +
	`overlays|overlay|render|setup|speaking|stages|stream|watch|write|docs|events|current\.stream|` +
	`openapi\.json|[a-z-]+\.json`

var (
	// Attribute values that are root-relative paths (not //host).
	attrLinkRE = regexp.MustCompile(`(\b(?:href|src|action|poster)\s*=\s*["'])/([^/]|["'])`)
	// CSS url(/…).
	cssURLRE = regexp.MustCompile(`(url\(\s*["']?)/([^/])`)
	// A quoted path to a dd-pits route inside a script: '/live', "/setup/x", `/overlay/…`.
	jsRouteRE = regexp.MustCompile("([\"'`])/((?:" + ddRoutes + ")(?:[/?#&\"'`]))")
	// fetch('/?…'), new EventSource('/…'), location.href = '/'.
	jsCallRE = regexp.MustCompile("((?:fetch|EventSource|location\\.(?:href|assign|replace))\\s*(?:\\(|=)\\s*[\"'`])/([^/]|[\"'`])")
)

// prefixLinks adds prefix to dd-pits' root-relative links in a page or script.
func prefixLinks(body []byte, prefix string) []byte {
	out := attrLinkRE.ReplaceAll(body, []byte("${1}"+prefix+"/${2}"))
	out = cssURLRE.ReplaceAll(out, []byte("${1}"+prefix+"/${2}"))
	out = jsRouteRE.ReplaceAll(out, []byte("${1}"+prefix+"/${2}"))
	out = jsCallRE.ReplaceAll(out, []byte("${1}"+prefix+"/${2}"))
	// The rules overlap on purpose (an href holding a route); undo any doubling.
	return bytes.ReplaceAll(out, []byte(prefix+prefix+"/"), []byte(prefix+"/"))
}

// withBasePath wraps the server for proxies that serve it under a sub-path.
func withBasePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := forwardedPrefix(r)
		if prefix == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Accept the path with or without the prefix: nginx strips it when
		// proxy_pass ends in a slash and keeps it otherwise.
		if r.URL.Path == prefix {
			r.URL.Path = "/"
		} else if strings.HasPrefix(r.URL.Path, prefix+"/") {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		}
		r.URL.RawPath = ""
		pw := &prefixWriter{ResponseWriter: w, prefix: prefix}
		next.ServeHTTP(pw, r)
		pw.finish()
	})
}

// prefixWriter rewrites links in text responses and passes everything else —
// the live event stream, the commentary audio — straight through.
type prefixWriter struct {
	http.ResponseWriter
	prefix  string
	decided bool
	rewrite bool
	status  int
	buf     bytes.Buffer
}

func (p *prefixWriter) decide() {
	if p.decided {
		return
	}
	p.decided = true
	h := p.Header()
	if loc := h.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
		h.Set("Location", p.prefix+loc)
	}
	ct := h.Get("Content-Type")
	p.rewrite = h.Get("Content-Encoding") == "" &&
		(strings.HasPrefix(ct, "text/html") || strings.Contains(ct, "javascript") || strings.HasPrefix(ct, "text/css"))
	if p.rewrite {
		h.Del("Content-Length")
	}
}

func (p *prefixWriter) WriteHeader(code int) {
	p.decide()
	if p.rewrite {
		p.status = code
		return
	}
	p.ResponseWriter.WriteHeader(code)
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if !p.decided {
		if p.Header().Get("Content-Type") == "" {
			p.Header().Set("Content-Type", http.DetectContentType(b))
		}
		p.decide()
	}
	if p.rewrite {
		return p.buf.Write(b)
	}
	return p.ResponseWriter.Write(b)
}

// Flush keeps streaming responses streaming.
func (p *prefixWriter) Flush() {
	if p.rewrite {
		return
	}
	if f, ok := p.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *prefixWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := p.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// finish writes a buffered, rewritten response. Called once the handler returns.
func (p *prefixWriter) finish() {
	if !p.rewrite {
		return
	}
	body := prefixLinks(p.buf.Bytes(), p.prefix)
	p.ResponseWriter.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if p.status == 0 {
		p.status = http.StatusOK
	}
	p.ResponseWriter.WriteHeader(p.status)
	_, _ = p.ResponseWriter.Write(body)
}
