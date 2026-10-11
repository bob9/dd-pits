// Command site serves the dd-pits promo website. Templates and static assets
// are embedded, so the whole site ships as one binary.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const siteURL = "https://dd-pits.threedigitalllamas.com.au"

// assetHashes maps a static path ("img/hero.webp") to a short content hash,
// appended as ?v= so assets can be cached forever and still bust on change.
var assetHashes = map[string]string{}

func init() {
	mime.AddExtensionType(".webp", "image/webp")
	_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := staticFS.ReadFile(p)
		sum := sha256.Sum256(b)
		assetHashes[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:])[:10]
		return nil
	})
}

// asset returns the versioned URL of a static file. An unknown path panics at
// render time, which the test turns into a failure rather than a broken image.
func asset(p string) (string, error) {
	h, ok := assetHashes[p]
	if !ok {
		return "", fmt.Errorf("unknown asset %q", p)
	}
	return "/static/" + p + "?v=" + h, nil
}

func parseTemplates() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{
		"asset": asset,
		"year":  func() int { return time.Now().Year() },
	}).ParseFS(templateFS, "templates/*.html"))
}

type pageData struct {
	SiteURL string
	Links   map[string]string
}

var data = pageData{
	SiteURL: siteURL,
	Links: map[string]string{
		"github":        "https://github.com/bob9/dd-pits",
		"releases":      "https://github.com/bob9/dd-pits/releases",
		"netpack":       "https://github.com/bob9/elrs-netpack",
		"netpackOrigin": "https://github.com/i-am-grub/elrs-netpack",
		"coffee":        "https://buymeacoffee.com/bobnine",
		"fpvtrackside":  "https://fpvtrackside.com",
		"tdl":           "https://threedigitalllamas.com.au",
		"demo":          "https://demo.dd-pits.threedigitalllamas.com.au",
	},
}

func newHandler() http.Handler {
	tmpl := parseTemplates()
	// Render once at startup: the page is static, and a template error should
	// stop the deploy rather than surface to a visitor.
	var home bytes.Buffer
	if err := tmpl.ExecuteTemplate(&home, "home.html", data); err != nil {
		log.Fatalf("render home: %v", err)
	}
	homeBytes := home.Bytes()

	sub, _ := fs.Sub(staticFS, "static")
	static := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "User-agent: *\nAllow: /\nSitemap: %s/sitemap.xml\n", siteURL)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>%s/</loc><changefreq>weekly</changefreq></url></urlset>
`, siteURL)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		u, _ := asset("img/favicon-32.png")
		http.Redirect(w, r, u, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write(homeBytes)
	})
	return securityHeaders(gzipText(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'self'")
		next.ServeHTTP(w, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
	use     bool
}

func (g *gzipWriter) WriteHeader(code int) {
	g.decide()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	ct := g.Header().Get("Content-Type")
	if ct == "" {
		return
	}
	for _, t := range []string{"text/", "application/javascript", "application/xml", "image/svg"} {
		if strings.HasPrefix(ct, t) {
			g.use = true
		}
	}
	if g.use {
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.decide()
	}
	if g.use {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func gzipText(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		// FileServer sets Content-Type from the extension before writing, so
		// set it up front for static files to let decide() see it.
		if strings.HasPrefix(r.URL.Path, "/static/") {
			if ct := mime.TypeByExtension(path.Ext(r.URL.Path)); ct != "" {
				w.Header().Set("Content-Type", ct)
			}
		}
		g := &gzipWriter{ResponseWriter: w}
		next.ServeHTTP(g, r)
		if g.gz != nil {
			g.gz.Close()
		}
	})
}

func main() {
	addr := flag.String("addr", "", "listen address (default 127.0.0.1:$PORT, PORT defaults to 9897)")
	flag.Parse()
	if *addr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "9897"
		}
		*addr = "127.0.0.1:" + port
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("dd-pits site listening on http://%s", *addr)
	log.Fatal(srv.ListenAndServe())
}
