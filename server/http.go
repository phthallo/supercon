package main

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

//go:embed web
var webFS embed.FS

// Written by tools/build.py --release.
//
//go:embed firmware
var firmwareFS embed.FS

const (
	cookieName     = "ink_id"
	maxUploadBytes = 8 << 20
)

type Server struct {
	store         *Store
	limiter       *Limiter
	hub           *Hub
	rhythm        *Rhythm
	legacyHosts   map[string]string
	adminPassword string
	ipHeader      string
	publicPaint   bool
	// Prefilled on /install. Public: anyone can read it from the page.
	wifiSSID     string
	wifiPassword string
}

func (s *Server) Routes() http.Handler {
	web, _ := fs.Sub(webFS, "web")
	static := http.FileServerFS(web)
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.page(web, "home.html"))
	mux.HandleFunc("GET /admin", s.admin(s.page(web, "admin.html")))
	mux.HandleFunc("GET /ink", s.page(web, "ink.html"))
	mux.Handle("GET /ink/{$}", http.RedirectHandler("/ink", http.StatusMovedPermanently))
	mux.HandleFunc("GET /ink/install", s.page(web, "install.html"))
	mux.HandleFunc("GET /quadrant", s.page(web, "quadrant.html"))
	mux.Handle("GET /quadrant/{$}", http.RedirectHandler("/quadrant", http.StatusMovedPermanently))
	mux.HandleFunc("GET /quadrant/play", s.page(web, "play.html"))
	mux.HandleFunc("GET /quadrant/c/{code}", s.page(web, "claim.html"))
	mux.HandleFunc("GET /api/install", s.handleInstall)
	mux.Handle("GET /static/", http.StripPrefix("/static/", static))
	firmware, _ := fs.Sub(firmwareFS, "firmware")
	mux.Handle("GET /install/files/", http.StripPrefix("/install/files/", noCache(http.FileServerFS(firmware))))

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/canvas", s.handleCanvas)
	mux.HandleFunc("POST /api/pixel", s.handlePixel)
	mux.HandleFunc("GET /api/widgets", s.handleWidgets)
	mux.HandleFunc("GET /api/widgets.png", s.handleWidgetLayer)
	mux.Handle("GET /ws", s.hub)

	mux.HandleFunc("POST /api/admin/widgets", s.admin(s.handleCreateWidget))
	mux.HandleFunc("PATCH /api/admin/widgets/{id}", s.admin(s.handleUpdateWidget))
	mux.HandleFunc("DELETE /api/admin/widgets/{id}", s.admin(s.handleDeleteWidget))
	mux.HandleFunc("POST /api/admin/widgets/{id}/image", s.admin(s.handleWidgetImage))
	mux.HandleFunc("POST /api/admin/canvas/clear", s.admin(s.handleClearCanvas))
	mux.HandleFunc("GET /api/admin/painters", s.admin(s.handlePainters))
	mux.HandleFunc("GET /api/admin/painters/cells", s.admin(s.handlePainterCells))
	mux.HandleFunc("POST /api/admin/painters/remove", s.admin(s.handleRemovePainter))
	mux.HandleFunc("POST /api/admin/painters/block", s.admin(s.handleBlockPainter))

	mux.HandleFunc("GET /badge/frame", s.handleFrame)
	mux.HandleFunc("GET /badge/frame.png", s.handleFramePNG)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	if s.rhythm == nil {
		return sameOrigin(s.blockBanned(mux))
	}
	s.rhythmAPI(mux)
	return sameOrigin(s.blockBanned(s.redirectLegacyHosts(mux)))
}

// legacyHostPaths maps the domains ink and Quadrant used to have to their paths on the main site.
func legacyHostPaths(ink, quadrant string) map[string]string {
	hosts := map[string]string{}
	if ink != "" {
		hosts[strings.ToLower(ink)] = "/ink"
	}
	if quadrant != "" {
		hosts[strings.ToLower(quadrant)] = "/quadrant"
	}
	return hosts
}

// redirectLegacyHosts sends browsers on an old domain to the main site. Badges flashed before
// the move keep calling /api and /badge there and cannot follow redirects.
func (s *Server) redirectLegacyHosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		prefix, legacy := s.legacyHosts[strings.ToLower(host)]
		if !legacy || strings.EqualFold(host, s.rhythm.host) || !isPageRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, s.rhythm.baseURL+legacyTarget(prefix, r.URL), http.StatusFound)
	})
}

func isPageRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	if p == "/ws" || p == "/healthz" {
		return false
	}
	for _, prefix := range []string{"/api/", "/badge/", "/static/", "/install/files/"} {
		if strings.HasPrefix(p, prefix) {
			return false
		}
	}
	return true
}

func legacyTarget(prefix string, u *url.URL) string {
	if u.Path == "/admin" {
		return "/admin#" + strings.TrimPrefix(prefix, "/")
	}
	path := u.EscapedPath()
	switch {
	case u.Path == "/":
		path = prefix
	// New badge builds show the /quadrant path, even when configured with the old domain.
	case u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/"):
		path = prefix + path
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

// blockBanned refuses banned IPs everywhere except admin (and the static files
// it loads) and the badge, which may share an IP with the person being blocked.
func (s *Server) blockBanned(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		open := p == "/admin" || p == "/healthz"
		for _, prefix := range []string{"/api/admin/", "/badge/", "/static/"} {
			open = open || strings.HasPrefix(p, prefix)
		}
		if !open && s.store.Blocked(s.clientIP(r)) {
			http.Error(w, "blocked", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin blocks cross-site writes. Admin uses basic auth, which browsers attach
// to cross-site form posts, so this check is what stops CSRF there.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin == "" {
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					http.Error(w, "cross-site request", http.StatusForbidden)
					return
				}
			} else if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) page(web fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.cookie(w, r)
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web, name)
	}
}

// noCache makes browsers revalidate, so a new release is picked up straight away.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if s.adminPassword == "" || !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminPassword)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="supercon admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) clientIP(r *http.Request) string {
	if s.ipHeader != "" {
		if v := r.Header.Get(s.ipHeader); v != "" {
			// The proxy appends the address it saw; earlier entries are client-supplied.
			parts := strings.Split(v, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) cookie(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(cookieName); err == nil && len(c.Value) == 32 {
		return c.Value
	}
	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	return id
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func seconds(d interface{ Seconds() float64 }) float64 {
	return math.Ceil(d.Seconds()*10) / 10
}

func (s *Server) handleInstall(w http.ResponseWriter, r *http.Request) {
	networks := []map[string]string{}
	if s.wifiSSID != "" {
		networks = append(networks, map[string]string{"ssid": s.wifiSSID, "password": s.wifiPassword})
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": networks})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := s.cookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"retry_after": seconds(s.limiter.Wait(s.clientIP(r), id)),
		"cooldown":    seconds(s.limiter.cooldown),
		"paint":       s.publicPaint,
		"grid":        [2]int{GridW, GridH},
		"panel":       [2]int{PanelW, PanelH},
		"widgets_v":   s.store.WidgetVersion(),
		"online":      s.hub.Count(),
	})
}

func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(s.store.Canvas())
}

func (s *Server) handlePixel(w http.ResponseWriter, r *http.Request) {
	if !s.publicPaint {
		jsonError(w, http.StatusForbidden, "painting is turned off")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		jsonError(w, http.StatusUnsupportedMediaType, "send application/json")
		return
	}
	var body struct {
		P   [][2]int
		C   uint8
		RGB *int // dithered per cell; overrides C
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(body.P) == 0 || len(body.P) > MaxPaintCells {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("send 1 to %d squares at a time", MaxPaintCells))
		return
	}
	for _, p := range body.P {
		if p[0] < 0 || p[0] >= GridW || p[1] < 0 || p[1] >= GridH {
			jsonError(w, http.StatusBadRequest, "pixel out of range")
			return
		}
	}
	colorAt := solid(body.C)
	if rgb := body.RGB; rgb != nil {
		if *rgb < 0 || *rgb > RGBWhite {
			jsonError(w, http.StatusBadRequest, "bad colour")
			return
		}
		colorAt = func(x, y int) uint8 { return Ink(*rgb, x, y) }
	} else if body.C > Red {
		jsonError(w, http.StatusBadRequest, "bad colour")
		return
	}
	id, ip := s.cookie(w, r), s.clientIP(r)
	ok, wait := s.limiter.Take(ip, id)
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "cooling down", "retry_after": seconds(wait)})
		return
	}
	changed, err := s.store.Paint(body.P, colorAt, ip, id)
	if err != nil {
		s.limiter.Refund(ip, id)
		log.Printf("paint: %v", err)
		jsonError(w, http.StatusInternalServerError, "Failed to save. Try again in a moment.")
		return
	}
	for c, cells := range changed {
		s.hub.Publish(Event{T: "px", P: cells, C: c})
	}
	writeJSON(w, http.StatusOK, map[string]any{"retry_after": seconds(wait)})
}

func (s *Server) handleWidgets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.store.WidgetVersion(), "widgets": s.store.Widgets()})
}

func (s *Server) handleWidgetLayer(w http.ResponseWriter, r *http.Request) {
	img := s.store.WidgetLayer()
	w.Header().Set("Content-Type", "image/png")
	if r.URL.Query().Has("v") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	png.Encode(w, img)
}

func (s *Server) widgetChanged() {
	s.hub.Publish(Event{T: "widgets", V: s.store.WidgetVersion()})
}

func widgetID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (s *Server) handleCreateWidget(w http.ResponseWriter, r *http.Request) {
	var p WidgetPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil || p.Kind == nil {
		jsonError(w, http.StatusBadRequest, "need json with kind")
		return
	}
	nw := NewWidget(*p.Kind)
	p.apply(&nw)
	created, err := s.store.CreateWidget(nw)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.widgetChanged()
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateWidget(w http.ResponseWriter, r *http.Request) {
	id, ok := widgetID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	var p WidgetPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json")
		return
	}
	updated, err := s.store.UpdateWidget(id, p, nil)
	s.respondUpdate(w, updated, err)
}

func (s *Server) handleWidgetImage(w http.ResponseWriter, r *http.Request) {
	id, ok := widgetID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "Upload an image under 8 MB.")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "upload failed")
		return
	}
	if err := CheckImage(data); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateWidget(id, WidgetPatch{}, data)
	s.respondUpdate(w, updated, err)
}

func (s *Server) respondUpdate(w http.ResponseWriter, updated Widget, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		jsonError(w, http.StatusNotFound, "no such widget")
	case err != nil:
		jsonError(w, http.StatusBadRequest, err.Error())
	default:
		s.widgetChanged()
		writeJSON(w, http.StatusOK, updated)
	}
}

func (s *Server) handleDeleteWidget(w http.ResponseWriter, r *http.Request) {
	id, ok := widgetID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.DeleteWidget(id); errors.Is(err, ErrNotFound) {
		jsonError(w, http.StatusNotFound, "no such widget")
		return
	} else if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.widgetChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearCanvas(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearCanvas(); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.Publish(Event{T: "clear"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePainters(w http.ResponseWriter, r *http.Request) {
	painters, err := s.store.Painters()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"painters": painters, "you": s.clientIP(r)})
}

func (s *Server) handlePainterCells(w http.ResponseWriter, r *http.Request) {
	cells, err := s.store.PainterCells(r.URL.Query().Get("ip"))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cells": cells})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func (s *Server) handleRemovePainter(w http.ResponseWriter, r *http.Request) {
	var body struct{ IP string }
	if !readJSON(w, r, &body) {
		return
	}
	if body.IP == "" {
		jsonError(w, http.StatusBadRequest, "missing ip")
		return
	}
	changed, err := s.store.RemovePainter(body.IP)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for c, cells := range changed {
		s.hub.Publish(Event{T: "px", P: cells, C: c})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBlockPainter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP      string
		Blocked bool
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.IP == "" {
		jsonError(w, http.StatusBadRequest, "missing ip")
		return
	}
	if err := s.store.SetBlocked(body.IP, body.Blocked); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFrame serves the badge planes. Content-Length is always set because
// MicroPython's requests cannot read chunked responses.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	frame, _, etag := s.store.Frame()
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/zlib")
	w.Header().Set("Content-Length", strconv.Itoa(len(frame)))
	w.Write(frame)
}

func (s *Server) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	_, img, etag := s.store.Frame()
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	png.Encode(w, img)
}
