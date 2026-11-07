package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"rsc.io/qr"
)

//go:embed songs/*.json
var demoSongsFS embed.FS

const (
	claimTTL      = time.Hour
	maxListeners  = 500
	maxAudioBytes = 20 << 20
	maxSongBytes  = 1 << 20
	maxNameRunes  = 16
	boardSize     = 100
)

var diffs = []string{"easy", "normal", "hard"}

// No 0/O or 1/I/L, so a code read off the badge types back unambiguously.
const codeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Note is [ms, lane] for a tap or [ms, lane, dur_ms] for a hold.
type Note []int

type Song struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Artist     string            `json:"artist"`
	BPM        float64           `json:"bpm"`
	OffsetMS   int               `json:"offset_ms"`
	DurationMS int               `json:"duration_ms"`
	HasAudio   bool              `json:"has_audio"`
	Notes      map[string]int    `json:"notes"`
	Charts     map[string][]Note `json:"charts,omitempty"`
}

// PlayerState is the latest command the badge sent, as the web player receives it.
type PlayerState struct {
	T       string  `json:"t"` // idle, start, pause, resume, stop or result
	Seq     int64   `json:"seq"`
	Song    string  `json:"song,omitempty"`
	Diff    string  `json:"diff,omitempty"`
	StartAt float64 `json:"start_at,omitempty"` // server ms at which audio position 0 plays
	PosMS   float64 `json:"pos_ms,omitempty"`
	// No claim code: anyone can watch, so only the badge screen shows it.
	Score   int     `json:"score,omitempty"`
	Acc     float64 `json:"acc,omitempty"`
	Grade   string  `json:"grade,omitempty"`
}

type Rhythm struct {
	db       *sql.DB
	baseURL  string
	host     string
	badgeKey string
	now      func() time.Time

	mu    sync.RWMutex
	songs map[string]*Song

	// One badge drives one web player, in memory only.
	playerMu  sync.Mutex
	player    PlayerState
	listeners map[chan PlayerState]struct{}

	runLimit, claimLimit *windowLimiter
}

func newRhythm(db *sql.DB, baseURL, badgeKey string) (*Rhythm, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("QUADRANT_URL %q is not an absolute URL", baseURL)
	}
	r := &Rhythm{
		db: db, baseURL: strings.TrimRight(baseURL, "/"), host: u.Hostname(), badgeKey: badgeKey, now: time.Now,
		songs: map[string]*Song{}, player: PlayerState{T: "idle"}, listeners: map[chan PlayerState]struct{}{},
	}
	r.runLimit = newWindowLimiter(30, time.Minute, r.clock)
	r.claimLimit = newWindowLimiter(20, time.Minute, r.clock)
	return r, nil
}

// OpenRhythm creates the Quadrant tables, adds the demo songs if missing and loads every song.
func OpenRhythm(db *sql.DB, baseURL, badgeKey string) (*Rhythm, error) {
	r, err := newRhythm(db, baseURL, badgeKey)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS rhythm_songs (
	id TEXT PRIMARY KEY, title TEXT NOT NULL, artist TEXT NOT NULL, bpm DOUBLE PRECISION NOT NULL,
	offset_ms INTEGER NOT NULL, duration_ms INTEGER NOT NULL, charts JSONB NOT NULL,
	audio BYTEA, audio_type TEXT NOT NULL DEFAULT '', updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS rhythm_runs (
	id BIGSERIAL PRIMARY KEY, code TEXT NOT NULL UNIQUE, song TEXT NOT NULL, diff TEXT NOT NULL,
	score INTEGER NOT NULL, acc DOUBLE PRECISION NOT NULL, grade TEXT NOT NULL,
	perfect INTEGER NOT NULL, great INTEGER NOT NULL, good INTEGER NOT NULL, miss INTEGER NOT NULL,
	max_combo INTEGER NOT NULL, notes INTEGER NOT NULL, ip TEXT NOT NULL, created_at BIGINT NOT NULL,
	name TEXT, claimed_at BIGINT);
CREATE INDEX IF NOT EXISTS rhythm_runs_board ON rhythm_runs (song, diff, score DESC) WHERE name IS NOT NULL;`); err != nil {
		return nil, err
	}
	seeds, err := demoSongs()
	if err != nil {
		return nil, err
	}
	// The demo songs follow the files in songs/ on every start, so a chart change there reaches the database.
	for _, s := range seeds {
		charts, _ := json.Marshal(s.Charts)
		if _, err := db.Exec(`INSERT INTO rhythm_songs (id, title, artist, bpm, offset_ms, duration_ms, charts, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (id) DO UPDATE SET title = excluded.title, artist = excluded.artist, bpm = excluded.bpm,
				offset_ms = excluded.offset_ms, duration_ms = excluded.duration_ms, charts = excluded.charts`,
			s.ID, s.Title, s.Artist, s.BPM, s.OffsetMS, s.DurationMS, string(charts), r.now().Unix()); err != nil {
			return nil, err
		}
	}
	rows, err := db.Query(`SELECT id, title, artist, bpm, offset_ms, duration_ms, charts, audio IS NOT NULL FROM rhythm_songs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := &Song{}
		var charts []byte
		if err := rows.Scan(&s.ID, &s.Title, &s.Artist, &s.BPM, &s.OffsetMS, &s.DurationMS, &charts, &s.HasAudio); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(charts, &s.Charts); err != nil {
			return nil, fmt.Errorf("song %s: %w", s.ID, err)
		}
		s.countNotes()
		r.songs[s.ID] = s
	}
	return r, rows.Err()
}

func (r *Rhythm) clock() time.Time { return r.now() }

func (r *Rhythm) serverMS() float64 { return float64(r.now().UnixMicro()) / 1000 }

func demoSongs() ([]*Song, error) {
	files, err := fs.Glob(demoSongsFS, "songs/*.json")
	if err != nil {
		return nil, err
	}
	var out []*Song
	for _, f := range files {
		data, err := demoSongsFS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		s, err := parseSong(strings.TrimSuffix(path.Base(f), ".json"), data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// parseSong reads the badge's song file format and validates it.
func parseSong(id string, data []byte) (*Song, error) {
	var in struct {
		Title      string                 `json:"title"`
		Artist     string                 `json:"artist"`
		BPM        float64                `json:"bpm"`
		OffsetMS   int                    `json:"offset_ms"`
		DurationMS int                    `json:"duration_ms"`
		Charts     map[string][][]float64 `json:"charts"`
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("bad song json: %w", err)
	}
	s := &Song{ID: id, Title: strings.TrimSpace(in.Title), Artist: strings.TrimSpace(in.Artist),
		BPM: in.BPM, OffsetMS: in.OffsetMS, DurationMS: in.DurationMS, Charts: map[string][]Note{}}
	switch {
	case !slugRE.MatchString(id):
		return nil, errors.New("id must be 1-40 lowercase letters, digits or dashes")
	case s.Title == "" || len(s.Title) > 80 || len(s.Artist) > 80:
		return nil, errors.New("title must be 1-80 characters and artist at most 80")
	case s.BPM < 20 || s.BPM > 400:
		return nil, errors.New("bpm must be 20-400")
	case s.DurationMS <= 0 || s.DurationMS > 20*60*1000:
		return nil, errors.New("duration_ms must be 1 ms to 20 minutes")
	case s.OffsetMS < -10000 || s.OffsetMS > 10000:
		return nil, errors.New("offset_ms must be within 10 s")
	}
	for diff, notes := range in.Charts {
		if !validDiff(diff) {
			return nil, fmt.Errorf("unknown difficulty %q", diff)
		}
		if len(notes) > 5000 {
			return nil, fmt.Errorf("%s has more than 5000 notes", diff)
		}
		chart := make([]Note, 0, len(notes))
		for _, n := range notes {
			end := 0.0
			if len(n) == 3 {
				end = n[2]
			}
			if (len(n) != 2 && len(n) != 3) || n[0] < 0 || end < 0 || n[0]+end > float64(s.DurationMS)+10000 ||
				n[1] < 0 || n[1] > 3 || n[1] != math.Trunc(n[1]) {
				return nil, fmt.Errorf("%s: note %v must be [ms, lane 0-3] or [ms, lane 0-3, dur_ms] within the song", diff, n)
			}
			note := Note{int(n[0]), int(n[1])}
			if len(n) == 3 {
				note = append(note, int(n[2]))
			}
			chart = append(chart, note)
		}
		sort.SliceStable(chart, func(i, j int) bool { return chart[i][0] < chart[j][0] })
		s.Charts[diff] = chart
	}
	if len(s.Charts) == 0 {
		return nil, errors.New("song has no charts")
	}
	s.countNotes()
	return s, nil
}

func validDiff(d string) bool {
	for _, x := range diffs {
		if d == x {
			return true
		}
	}
	return false
}

// countNotes sets Notes to the number of judgments per chart: a hold with
// dur_ms > 0 is judged twice, at its head and its tail.
func (s *Song) countNotes() {
	s.Notes = map[string]int{}
	for d, c := range s.Charts {
		s.Notes[d] = judgments(c)
	}
}

func judgments(chart []Note) int {
	n := len(chart)
	for _, note := range chart {
		if len(note) == 3 && note[2] > 0 {
			n++
		}
	}
	return n
}

func (r *Rhythm) song(id string) *Song {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.songs[id]
}

func (r *Rhythm) songList() []Song {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Song, 0, len(r.songs))
	for _, s := range r.songs {
		c := *s
		c.Charts = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

func randomCode(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		// 256 % 31 leaves a small bias, which is fine for codes that are not secrets.
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// Scoring, as on the badge.

// osu!mania accuracy weights for perfect, great, good and miss.
var judgePoints = [4]int{300, 200, 100, 0}

// gradeFor follows osu!standard: the share of perfects sets the grade, and a miss caps it at A.
func gradeFor(counts [4]int) string {
	n := counts[0] + counts[1] + counts[2] + counts[3]
	over := func(tenths int) bool { return counts[0]*10 > tenths*n }
	clean := counts[3] == 0
	switch {
	case over(9) && clean && counts[2]*100 < n:
		return "S"
	case over(8) && clean || over(9):
		return "A"
	case over(7) && clean || over(8):
		return "B"
	case over(6):
		return "C"
	}
	return "D"
}

type runScore struct {
	Score int
	Acc   float64
	Grade string
}

// scoreRun recomputes a finished run from its judgement counts (perfect, great,
// good, miss) and max combo, after checking they are possible for an n-note chart.
func scoreRun(n int, counts [4]int, maxCombo int) (runScore, error) {
	if n <= 0 {
		return runScore{}, errors.New("chart has no notes")
	}
	sum, points := 0, 0
	for i, c := range counts {
		if c < 0 {
			return runScore{}, errors.New("counts must not be negative")
		}
		sum += c
		points += c * judgePoints[i]
	}
	if sum != n {
		return runScore{}, fmt.Errorf("counts add up to %d, chart has %d judgments", sum, n)
	}
	misses := counts[3]
	hits := n - misses
	// m misses split the hits into at most m+1 streaks, so the longest is at least hits/(m+1).
	if maxCombo > hits || maxCombo < (hits+misses)/(misses+1) {
		return runScore{}, fmt.Errorf("max_combo %d is impossible with %d hits and %d misses", maxCombo, hits, misses)
	}
	score := int(float64(900000*points)/float64(300*n) + float64(100000*maxCombo)/float64(n))
	return runScore{Score: score, Acc: float64(points) / float64(300*n), Grade: gradeFor(counts)}, nil
}

// checkClaimed compares the badge's numbers with the server's.
func checkClaimed(want runScore, score int, acc float64, grade string) error {
	// MicroPython on the badge uses 32-bit floats, so its score can be off by one.
	if d := score - want.Score; d < -1 || d > 1 {
		return fmt.Errorf("score %d does not match counts (want %d)", score, want.Score)
	}
	if math.Abs(acc-want.Acc) > 1e-4 {
		return fmt.Errorf("acc %v does not match counts (want %.4f)", acc, want.Acc)
	}
	if grade != want.Grade {
		return fmt.Errorf("grade %q does not match counts (want %s)", grade, want.Grade)
	}
	return nil
}

// Names.

var nameBlocklist = []string{
	"fuck", "shit", "cunt", "nigg", "fagg", "faggot", "retard", "whore", "bitch",
	"penis", "vagina", "rape", "nazi", "hitler", "kike", "tranny", "dick", "pussy", "slut",
}

var leet = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s")

// checkName trims and validates a leaderboard name.
func checkName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if name == "" {
		return "", errors.New("Enter a name.")
	}
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", fmt.Errorf("Names are at most %d characters.", maxNameRunes)
	}
	for _, c := range name {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !strings.ContainsRune(" -_.'", c) {
			return "", errors.New("Use letters, numbers, spaces and - _ . ' only.")
		}
	}
	var flat strings.Builder
	for _, c := range leet.Replace(strings.ToLower(name)) {
		if unicode.IsLetter(c) {
			flat.WriteRune(c)
		}
	}
	for _, w := range nameBlocklist {
		if strings.Contains(flat.String(), w) {
			return "", errors.New("Pick a different name.")
		}
	}
	return name, nil
}

// isBadge reports whether a request carries the badge key as "Authorization: Bearer KEY".
func (r *Rhythm) isBadge(req *http.Request) bool {
	key, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return ok && r.badgeKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(r.badgeKey)) == 1
}

// command applies a badge command to the player and pushes the new state to its listeners.
func (r *Rhythm) command(apply func(*PlayerState) error) (int, error) {
	r.playerMu.Lock()
	defer r.playerMu.Unlock()
	next := r.player
	if err := apply(&next); err != nil {
		return 0, err
	}
	next.Seq = r.player.Seq + 1
	r.player = next
	for ch := range r.listeners {
		select {
		case <-ch:
		default:
		}
		ch <- next
	}
	return len(r.listeners), nil
}

// subscribe returns a channel holding the player's latest state.
func (r *Rhythm) subscribe() (<-chan PlayerState, PlayerState, func(), error) {
	r.playerMu.Lock()
	defer r.playerMu.Unlock()
	if len(r.listeners) >= maxListeners {
		return nil, PlayerState{}, nil, errors.New("too many players open")
	}
	ch := make(chan PlayerState, 1)
	r.listeners[ch] = struct{}{}
	cancel := func() {
		r.playerMu.Lock()
		defer r.playerMu.Unlock()
		if _, ok := r.listeners[ch]; ok {
			delete(r.listeners, ch)
			close(ch)
		}
	}
	return ch, r.player, cancel, nil
}

// windowLimiter allows limit requests per key in each fixed window. Badges at
// the same event share one public IP, so this is a ceiling and not a cooldown.
type windowLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu     sync.Mutex
	start  time.Time
	counts map[string]int
}

func newWindowLimiter(limit int, window time.Duration, now func() time.Time) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, now: now, counts: map[string]int{}}
}

func (l *windowLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.Sub(l.start) >= l.window {
		l.start, l.counts = now, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.limit
}

// HTTP.

func (s *Server) isQuadrantHost(r *http.Request) bool {
	if s.rhythm == nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.EqualFold(host, s.rhythm.host) || strings.EqualFold(host, "quadrant.localhost")
}

func (s *Server) byHost(ink, quadrant http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isQuadrantHost(r) {
			quadrant.ServeHTTP(w, r)
			return
		}
		ink.ServeHTTP(w, r)
	})
}

func (s *Server) quadrantRoutes(web fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page(web, "quadrant.html"))
	mux.HandleFunc("GET /play", s.page(web, "play.html"))
	mux.HandleFunc("GET /c/{code}", s.page(web, "claim.html"))
	mux.HandleFunc("GET /admin", s.admin(s.page(web, "quadrant-admin.html")))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	s.rhythmAPI(mux)
	return mux
}

// rhythmAPI is mounted on both hosts so a badge can use whichever one it was given.
func (s *Server) rhythmAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/rhythm/time", s.handleRhythmTime)
	mux.HandleFunc("GET /api/rhythm/songs", s.handleSongs)
	mux.HandleFunc("GET /api/rhythm/songs/{id}", s.handleSong)
	mux.HandleFunc("GET /api/rhythm/songs/{id}/audio", s.handleSongAudio)
	mux.HandleFunc("GET /api/rhythm/player/events", s.handlePlayerEvents)
	mux.HandleFunc("POST /api/rhythm/player/{action}", s.handlePlayerCommand)
	mux.HandleFunc("POST /api/rhythm/runs", s.handleCreateRun)
	mux.HandleFunc("GET /api/rhythm/claims/{code}", s.handleClaim)
	mux.HandleFunc("POST /api/rhythm/claims/{code}", s.handleClaimRun)
	mux.HandleFunc("GET /api/rhythm/leaderboard", s.handleLeaderboard)

	mux.HandleFunc("POST /api/admin/rhythm/songs", s.admin(s.handleUploadSong))
	mux.HandleFunc("DELETE /api/admin/rhythm/songs/{id}", s.admin(s.handleDeleteSong))
	mux.HandleFunc("GET /api/admin/rhythm/runs", s.admin(s.handleRecentRuns))
	mux.HandleFunc("DELETE /api/admin/rhythm/runs/{id}", s.admin(s.handleDeleteRun))
}

// sendJSON sets Content-Length: MicroPython's requests cannot read chunked responses.
func sendJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

func sendError(w http.ResponseWriter, status int, msg string) {
	sendJSON(w, status, map[string]string{"error": msg})
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(v); err != nil {
		sendError(w, http.StatusBadRequest, "bad json")
		return false
	}
	return true
}

func (s *Server) handleRhythmTime(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// Whole ms: the badge parses JSON numbers with a fraction as 32-bit floats.
	sendJSON(w, http.StatusOK, map[string]int64{"t": int64(s.rhythm.serverMS())})
}

func (s *Server) handleSongs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	sendJSON(w, http.StatusOK, map[string]any{"songs": s.rhythm.songList()})
}

func (s *Server) handleSong(w http.ResponseWriter, r *http.Request) {
	song := s.rhythm.song(r.PathValue("id"))
	if song == nil {
		sendError(w, http.StatusNotFound, "no such song")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	sendJSON(w, http.StatusOK, song)
}

func (s *Server) handleSongAudio(w http.ResponseWriter, r *http.Request) {
	var audio []byte
	var typ string
	var updated int64
	err := s.rhythm.db.QueryRow(`SELECT audio, audio_type, updated_at FROM rhythm_songs WHERE id = $1 AND audio IS NOT NULL`,
		r.PathValue("id")).Scan(&audio, &typ, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no audio for this song", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("audio: %v", err)
		http.Error(w, "failed to load audio", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%d"`, r.PathValue("id"), updated))
	http.ServeContent(w, r, "", time.Unix(updated, 0), bytes.NewReader(audio))
}

func (s *Server) handlePlayerEvents(w http.ResponseWriter, r *http.Request) {
	ch, state, cancel, err := s.rhythm.subscribe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	send := func(st PlayerState) bool {
		b, _ := json.Marshal(st)
		fmt.Fprintf(w, "data: %s\n\n", b)
		return rc.Flush() == nil
	}
	if !send(state) {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case st, ok := <-ch:
			if !ok {
				return
			}
			if !send(st) {
				return
			}
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			if rc.Flush() != nil {
				return
			}
		}
	}
}

func (s *Server) handlePlayerCommand(w http.ResponseWriter, r *http.Request) {
	if !s.rhythm.isBadge(r) {
		sendError(w, http.StatusForbidden, "wrong badge key")
		return
	}
	var body struct {
		Song    string  `json:"song"`
		Diff    string  `json:"diff"`
		StartAt float64 `json:"start_at"`
		PosMS   float64 `json:"pos_ms"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	now := s.rhythm.serverMS()
	startAt := func() error {
		if math.Abs(body.StartAt-now) > 10*60*1000 {
			return fmt.Errorf("start_at must be within 10 minutes of server time %.0f", now)
		}
		return nil
	}
	var apply func(*PlayerState) error
	switch action := r.PathValue("action"); action {
	case "start":
		apply = func(st *PlayerState) error {
			if song := s.rhythm.song(body.Song); song == nil || song.Charts[body.Diff] == nil {
				return errors.New("unknown song or difficulty")
			}
			if err := startAt(); err != nil {
				return err
			}
			*st = PlayerState{T: action, Song: body.Song, Diff: body.Diff, StartAt: body.StartAt}
			return nil
		}
	case "resume":
		apply = func(st *PlayerState) error {
			if st.T != "pause" {
				return errors.New("nothing is paused")
			}
			if err := startAt(); err != nil {
				return err
			}
			*st = PlayerState{T: action, Song: st.Song, Diff: st.Diff, StartAt: body.StartAt}
			return nil
		}
	case "pause":
		apply = func(st *PlayerState) error {
			if st.T != "start" && st.T != "resume" {
				return errors.New("nothing is playing")
			}
			pos := body.PosMS
			if pos == 0 {
				pos = now - st.StartAt
			}
			*st = PlayerState{T: action, Song: st.Song, Diff: st.Diff, PosMS: pos}
			return nil
		}
	case "stop":
		apply = func(st *PlayerState) error {
			*st = PlayerState{T: action, Song: st.Song, Diff: st.Diff}
			return nil
		}
	default:
		sendError(w, http.StatusNotFound, "action must be start, pause, resume or stop")
		return
	}
	listeners, err := s.rhythm.command(apply)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	sendJSON(w, http.StatusOK, map[string]int{"listeners": listeners})
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Song     string  `json:"song"`
		Diff     string  `json:"diff"`
		Score    int     `json:"score"`
		Acc      float64 `json:"acc"`
		Grade    string  `json:"grade"`
		Counts   []int   `json:"counts"`
		MaxCombo int     `json:"max_combo"`
		Notes    int     `json:"notes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	ip := s.clientIP(r)
	if !s.rhythm.runLimit.Allow(ip) {
		sendError(w, http.StatusTooManyRequests, "too many runs; wait a minute")
		return
	}
	song := s.rhythm.song(body.Song)
	if song == nil || song.Charts[body.Diff] == nil {
		sendError(w, http.StatusBadRequest, "unknown song or difficulty")
		return
	}
	n := song.Notes[body.Diff]
	if body.Notes != n {
		sendError(w, http.StatusBadRequest, fmt.Sprintf("notes is %d, chart has %d judgments", body.Notes, n))
		return
	}
	if len(body.Counts) != 4 {
		sendError(w, http.StatusBadRequest, "counts must be [perfect, great, good, miss]")
		return
	}
	counts := [4]int(body.Counts)
	want, err := scoreRun(n, counts, body.MaxCombo)
	if err == nil {
		err = checkClaimed(want, body.Score, body.Acc, body.Grade)
	}
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.rhythm.now()
	if _, err := s.rhythm.db.Exec(`DELETE FROM rhythm_runs WHERE name IS NULL AND created_at < $1`, now.Add(-claimTTL).Unix()); err != nil {
		log.Printf("purge runs: %v", err)
	}
	var code string
	for range 3 {
		code = randomCode(8)
		_, err = s.rhythm.db.Exec(`INSERT INTO rhythm_runs (code, song, diff, score, acc, grade, perfect, great, good, miss, max_combo, notes, ip, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			code, song.ID, body.Diff, want.Score, want.Acc, want.Grade, counts[0], counts[1], counts[2], counts[3], body.MaxCombo, n, ip, now.Unix())
		if err == nil {
			break
		}
	}
	if err != nil {
		log.Printf("save run: %v", err)
		sendError(w, http.StatusInternalServerError, "failed to save run")
		return
	}
	claimURL := s.rhythm.baseURL + "/c/" + code
	if s.rhythm.isBadge(r) {
		s.rhythm.command(func(st *PlayerState) error {
			*st = PlayerState{T: "result", Song: song.ID, Diff: body.Diff, Score: want.Score, Acc: want.Acc, Grade: want.Grade}
			return nil
		})
	}
	rows, err := qrRows(claimURL)
	if err != nil {
		log.Printf("qr: %v", err)
	}
	sendJSON(w, http.StatusCreated, map[string]any{"code": code, "url": claimURL, "expires_s": int(claimTTL.Seconds()), "qr": rows})
}

// qrRows encodes s as a QR code for the badge, which has no encoder: one string
// per row, "1" for a dark module. The quiet zone is left to the badge.
func qrRows(s string) ([]string, error) {
	c, err := qr.Encode(s, qr.M)
	if err != nil {
		return nil, err
	}
	rows := make([]string, c.Size)
	var b strings.Builder
	for y := range c.Size {
		b.Reset()
		for x := range c.Size {
			if c.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		rows[y] = b.String()
	}
	return rows, nil
}

type runRow struct {
	ID       int64   `json:"id"`
	Code     string  `json:"code,omitempty"`
	Song     string  `json:"song"`
	Title    string  `json:"title"`
	Diff     string  `json:"diff"`
	Name     string  `json:"name,omitempty"`
	Score    int     `json:"score"`
	Acc      float64 `json:"acc"`
	Grade    string  `json:"grade"`
	Counts   [4]int  `json:"counts"`
	MaxCombo int     `json:"max_combo"`
	Notes    int     `json:"notes"`
	At       int64   `json:"at"`
	IP       string  `json:"ip,omitempty"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var run runRow
	var name sql.NullString
	err := s.rhythm.db.QueryRow(`SELECT id, code, song, diff, name, score, acc, grade, perfect, great, good, miss, max_combo, notes, created_at
		FROM rhythm_runs WHERE code = $1`, strings.ToUpper(r.PathValue("code"))).Scan(
		&run.ID, &run.Code, &run.Song, &run.Diff, &name, &run.Score, &run.Acc, &run.Grade,
		&run.Counts[0], &run.Counts[1], &run.Counts[2], &run.Counts[3], &run.MaxCombo, &run.Notes, &run.At)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !name.Valid && s.rhythm.expired(run.At)) {
		sendError(w, http.StatusNotFound, "This run expired or never existed.")
		return
	} else if err != nil {
		log.Printf("claim: %v", err)
		sendError(w, http.StatusInternalServerError, "Failed to load. Try again in a moment.")
		return
	}
	run.Name = name.String
	if song := s.rhythm.song(run.Song); song != nil {
		run.Title = song.Title
	}
	sendJSON(w, http.StatusOK, run)
}

func (r *Rhythm) expired(createdAt int64) bool {
	return r.now().Add(-claimTTL).Unix() > createdAt
}

func (s *Server) handleClaimRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !s.rhythm.claimLimit.Allow(s.clientIP(r)) {
		sendError(w, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
		return
	}
	name, err := checkName(body.Name)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	code := strings.ToUpper(r.PathValue("code"))
	now := s.rhythm.now()
	res, err := s.rhythm.db.Exec(`UPDATE rhythm_runs SET name = $1, claimed_at = $2
		WHERE code = $3 AND name IS NULL AND created_at >= $4`, name, now.Unix(), code, now.Add(-claimTTL).Unix())
	if err != nil {
		log.Printf("claim: %v", err)
		sendError(w, http.StatusInternalServerError, "Failed to save. Try again in a moment.")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var claimed bool
		err := s.rhythm.db.QueryRow(`SELECT name IS NOT NULL FROM rhythm_runs WHERE code = $1`, code).Scan(&claimed)
		if err == nil && claimed {
			sendError(w, http.StatusConflict, "This run is already on the leaderboard.")
		} else {
			sendError(w, http.StatusNotFound, "This run expired or never existed.")
		}
		return
	}
	sendJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	song, diff := s.rhythm.song(q.Get("song")), q.Get("diff")
	if song == nil || song.Charts[diff] == nil {
		sendError(w, http.StatusNotFound, "unknown song or difficulty")
		return
	}
	rows, err := s.rhythm.db.Query(`SELECT id, name, score, acc, grade, perfect, great, good, miss, max_combo, notes, claimed_at FROM (
			SELECT DISTINCT ON (lower(name)) * FROM rhythm_runs WHERE song = $1 AND diff = $2 AND name IS NOT NULL
			ORDER BY lower(name), score DESC, claimed_at) best
		ORDER BY score DESC, claimed_at LIMIT $3`, song.ID, diff, boardSize)
	if err != nil {
		log.Printf("leaderboard: %v", err)
		sendError(w, http.StatusInternalServerError, "failed to load leaderboard")
		return
	}
	defer rows.Close()
	runs := []runRow{}
	for rows.Next() {
		run := runRow{Song: song.ID, Title: song.Title, Diff: diff}
		if err := rows.Scan(&run.ID, &run.Name, &run.Score, &run.Acc, &run.Grade,
			&run.Counts[0], &run.Counts[1], &run.Counts[2], &run.Counts[3], &run.MaxCombo, &run.Notes, &run.At); err != nil {
			sendError(w, http.StatusInternalServerError, "failed to load leaderboard")
			return
		}
		runs = append(runs, run)
	}
	w.Header().Set("Cache-Control", "no-cache")
	sendJSON(w, http.StatusOK, map[string]any{"song": song.ID, "diff": diff, "runs": runs})
}

func (s *Server) handleUploadSong(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes+maxSongBytes+64<<10)
	if err := r.ParseMultipartForm(maxSongBytes); err != nil {
		sendError(w, http.StatusBadRequest, "Upload a song JSON and an optional mp3 or ogg under 20 MB.")
		return
	}
	readPart := func(field string, limit int64) ([]byte, error) {
		f, _, err := r.FormFile(field)
		if errors.Is(err, http.ErrMissingFile) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, limit+1))
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("%s is too large", field)
		}
		return data, err
	}
	songJSON, err := readPart("song", maxSongBytes)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	audio, err := readPart("audio", maxAudioBytes)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	typ := ""
	if audio != nil {
		if typ = audioType(audio); typ == "" {
			sendError(w, http.StatusBadRequest, "audio must be mp3 or ogg")
			return
		}
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" && songJSON != nil {
		var t struct{ Title string }
		json.Unmarshal(songJSON, &t)
		title = t.Title
	}
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		id = slugify(title)
	}
	var song *Song
	switch {
	case songJSON != nil:
		song, err = parseSong(id, songJSON)
	case typ != "audio/mpeg":
		err = errors.New("upload a song JSON, or an mp3 to generate charts from")
	case title == "":
		err = errors.New("title is required to generate charts")
	default:
		song, err = GenerateSong(id, title, strings.TrimSpace(r.FormValue("artist")), audio)
		if err == nil {
			// Round-trip through the file format so generated charts pass the same checks as uploads.
			data, _ := json.Marshal(map[string]any{"title": song.Title, "artist": song.Artist, "bpm": song.BPM,
				"offset_ms": song.OffsetMS, "duration_ms": song.DurationMS, "charts": song.Charts})
			song, err = parseSong(id, data)
		}
	}
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.rhythm.saveSong(song, audio, typ); err != nil {
		log.Printf("save song: %v", err)
		sendError(w, http.StatusInternalServerError, "failed to save song")
		return
	}
	sendJSON(w, http.StatusOK, s.rhythm.song(song.ID))
}

// saveSong inserts or replaces a song. A nil audio keeps the song's current audio.
func (r *Rhythm) saveSong(song *Song, audio []byte, typ string) error {
	charts, _ := json.Marshal(song.Charts)
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`INSERT INTO rhythm_songs (id, title, artist, bpm, offset_ms, duration_ms, charts, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET title = excluded.title, artist = excluded.artist, bpm = excluded.bpm,
			offset_ms = excluded.offset_ms, duration_ms = excluded.duration_ms, charts = excluded.charts, updated_at = excluded.updated_at
		RETURNING audio IS NOT NULL`,
		song.ID, song.Title, song.Artist, song.BPM, song.OffsetMS, song.DurationMS, string(charts), r.now().Unix()).Scan(&song.HasAudio); err != nil {
		return err
	}
	if audio != nil {
		if _, err := tx.Exec(`UPDATE rhythm_songs SET audio = $1, audio_type = $2 WHERE id = $3`, audio, typ, song.ID); err != nil {
			return err
		}
		song.HasAudio = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	r.songs[song.ID] = song
	r.mu.Unlock()
	return nil
}

func audioType(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("OggS")):
		return "audio/ogg"
	case bytes.HasPrefix(b, []byte("ID3")), len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return "audio/mpeg"
	}
	return ""
}

func slugify(title string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(title) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	return out
}

// handleDeleteSong removes the song but keeps its runs, so a re-upload under the same id restores its leaderboard.
func (s *Server) handleDeleteSong(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := s.rhythm.db.Exec(`DELETE FROM rhythm_songs WHERE id = $1`, id)
	if err != nil {
		sendError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendError(w, http.StatusNotFound, "no such song")
		return
	}
	s.rhythm.mu.Lock()
	delete(s.rhythm.songs, id)
	s.rhythm.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRecentRuns(w http.ResponseWriter, r *http.Request) {
	rows, err := s.rhythm.db.Query(`SELECT id, song, diff, name, score, acc, grade, max_combo, notes, claimed_at, ip
		FROM rhythm_runs WHERE name IS NOT NULL ORDER BY claimed_at DESC LIMIT 200`)
	if err != nil {
		sendError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	runs := []runRow{}
	for rows.Next() {
		var run runRow
		if err := rows.Scan(&run.ID, &run.Song, &run.Diff, &run.Name, &run.Score, &run.Acc, &run.Grade, &run.MaxCombo, &run.Notes, &run.At, &run.IP); err != nil {
			sendError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if song := s.rhythm.song(run.Song); song != nil {
			run.Title = song.Title
		}
		runs = append(runs, run)
	}
	sendJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		sendError(w, http.StatusBadRequest, "bad id")
		return
	}
	res, err := s.rhythm.db.Exec(`DELETE FROM rhythm_runs WHERE id = $1`, id)
	if err != nil {
		sendError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		sendError(w, http.StatusNotFound, "no such run")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
