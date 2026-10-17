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
	SiteURL   string
	Links     map[string]string
	Overlays  []siteOverlayGroup
	Downloads []siteDownload
}

type siteOverlay struct{ Img, Title, What string }

type siteOverlayGroup struct {
	Name  string
	Items []siteOverlay
}

// siteDownload is one platform's binary from the latest GitHub release. The
// asset names carry no version, so these links always fetch the newest build.
type siteDownload struct{ OS, Label, Note, Asset string }

var data = pageData{
	SiteURL: siteURL,
	Links: map[string]string{
		"github":        "https://github.com/bob9/dd-pits",
		"releases":      "https://github.com/bob9/dd-pits/releases",
		"netpack":       "https://github.com/bob9/elrs-netpack",
		"netpackOrigin": "https://github.com/i-am-grub/elrs-netpack",
		"coffee":        "https://buymeacoffee.com/bobnine",
		"fpvtrackside":  "https://fpvtrackside.com",
		"ftAlpha":       "https://alpha.fpvtrackside.com/",
		"hdzFirmware":   "https://github.com/bob9/hdzero-goggle",
		"hdzRelease":    "https://github.com/bob9/hdzero-goggle/releases/tag/v9.5.45",
		"bpRelease":     "https://github.com/bob9/Backpack/releases/tag/v2026.07.17-aufpv-race",
		"backpack":      "https://github.com/bob9/Backpack",
		"rotorhazard":   "https://github.com/RotorHazard/RotorHazard",
		"demoBracket":   "https://demo.dd-pits.threedigitalllamas.com.au/stages/s1",
		"rhplugin":      "https://github.com/bob9/dd-pits/releases/latest/download/dd-pits-rotorhazard-plugin.zip",
		"rhdocs":        "https://github.com/bob9/dd-pits#rotorhazard",
		"tdl":           "https://threedigitalllamas.com.au",
		"demo":          "https://demo.dd-pits.threedigitalllamas.com.au",
		"demoOverlays":  "https://demo.dd-pits.threedigitalllamas.com.au/overlays",
		"author":        "https://github.com/bob9",
		"latest":        "https://github.com/bob9/dd-pits/releases/latest/download/",
	},
	Downloads: []siteDownload{
		{"mac", "Mac · Apple Silicon", "M1 or later", "dd-pits-darwin-arm64"},
		{"mac-intel", "Mac · Intel", "Older Macs", "dd-pits-darwin-amd64"},
		{"windows", "Windows", "64-bit", "dd-pits-windows-amd64.exe"},
		{"linux", "Linux", "x86-64 PC", "dd-pits-linux-amd64"},
		{"linux-arm", "Raspberry Pi", "64-bit Pi OS", "dd-pits-linux-arm64"},
		{"linux-arm32", "Raspberry Pi", "32-bit Pi OS", "dd-pits-linux-arm"},
	},
	Overlays: []siteOverlayGroup{
		{"Live timing", []siteOverlay{
			{"scorebug", "Scorebug", "TV-style badge with the time of day; the heat strip slides in when a heat is cued, stays up while it races, and shows the result."},
			{"topbar", "Top bar", "Racing now / on deck / just raced, the race timer, the meeting and your club across the top."},
			{"corner", "Corner", "Logo, time of day and the heat, glowing red while it races."},
			{"race", "Race grid", "Transparent tiles over your FPV feeds, each with the pilot, position and last four laps."},
			{"node-2", "Pilot cam tag", "A lower third for one pilot's feed: position, channel, lap, last, best and total."},
			{"strip", "Racing now strip", "The pilots in the air, ringed in their channel colours."},
			{"lapchart", "Lap chart", "Every lap of the heat, the fastest highlighted."},
			{"fastest", "Fastest lap", "The quickest lap of the heat and who set it."},
			{"panel", "Race panel", "Positions, laps, totals and the clock in a compact panel."},
			{"lowerthird", "Lower third", "The lineup across the bottom, F1 style."},
		}},
		{"Between heats", []siteOverlay{
			{"strip-next", "Up next strip", "The next heat's pilots and channels, bottom left."},
			{"lineup-next", "Lineup", "A full-screen walk-on with each pilot's rank and best times."},
			{"lineup", "Lineup — racing", "The same scene once the heat runs: positions, totals and laps."},
			{"upnow-next", "Up Now", "Pilot cards walk on with rank, best lap and best consecutive, then step off. Loops."},
			{"banner-next", "Up Now banner", "The Up Now cards as a lower-third banner."},
		}},
		{"Standings", []siteOverlay{
			{"spotlight", "Spotlight", "One pilot's profile or two head to head — push it to stream from the gallery."},
			{"leaderboard", "Leaderboard", "The meeting standings with ▲▼ movement since the last heat."},
			{"laps", "Laps rail", "One time per pilot, 🔥 for whoever just improved."},
			{"standings", "Standings", "The leader as the hero, the rest of the field below. Live, then final."},
			{"podium", "Podium", "The top four on podium steps, the winner crowned."},
			{"results", "Results card", "A results scene from the URL alone — type the finishing order."},
			{"notice", "Notice", "Your own message: a weather hold, a sponsor, the next round."},
		}},
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
	var setup bytes.Buffer
	if err := tmpl.ExecuteTemplate(&setup, "setup.html", data); err != nil {
		log.Fatalf("render setup guide: %v", err)
	}
	setupBytes := setup.Bytes()

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
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>%s/</loc><changefreq>weekly</changefreq></url><url><loc>%s/best-setup</loc><changefreq>weekly</changefreq></url></urlset>
`, siteURL, siteURL)
	})
	mux.HandleFunc("/best-setup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Write(setupBytes)
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
