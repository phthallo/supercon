package main

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "golang.org/x/image/webp"
)

const (
	KindText  = "text"
	KindBox   = "box"
	KindImage = "image"

	maxImagePixels = 4096 * 4096
)

type Widget struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Z        int    `json:"z"`
	Color    int    `json:"color"`
	Fill     int    `json:"fill"` // -1 for none
	Border   int    `json:"border"`
	Text     string `json:"text"`
	FontSize int    `json:"font_size"`
	Font     string `json:"font"`
	Align    string `json:"align"`
	Grey     bool   `json:"grey"`
	HasImage bool   `json:"has_image"`

	src    []byte
	dither []byte
}

// WidgetPatch carries the fields an admin edit may change; nil leaves a field alone.
type WidgetPatch struct {
	Kind     *string `json:"kind"`
	X        *int    `json:"x"`
	Y        *int    `json:"y"`
	W        *int    `json:"w"`
	H        *int    `json:"h"`
	Z        *int    `json:"z"`
	Color    *int    `json:"color"`
	Fill     *int    `json:"fill"`
	Border   *int    `json:"border"`
	Text     *string `json:"text"`
	FontSize *int    `json:"font_size"`
	Font     *string `json:"font"`
	Align    *string `json:"align"`
	Grey     *bool   `json:"grey"`
}

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB

	mu      sync.RWMutex
	canvas  []byte
	widgets map[int64]*Widget
	epoch   int64
	version int64
	// Bumped only by widget edits; keys the web widget layer.
	widgetVersion int64
	// Last paint_log id before the most recent canvas clear.
	clearedAfter int64
	bans         map[string]bool

	frameMu      sync.Mutex
	frameVersion int64
	frame        []byte
	frameImg     *image.Paletted
	layerVersion int64
	layer        *image.Paletted
}

// OpenStore connects to Postgres and creates the tables. A non-empty sqlitePath is
// copied into the database once, if the database has no paint or widgets yet.
func OpenStore(url, sqlitePath string) (*Store, error) {
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS canvas (id INTEGER PRIMARY KEY CHECK (id = 1), data BYTEA NOT NULL);
CREATE TABLE IF NOT EXISTS paint_log (
	id BIGSERIAL PRIMARY KEY, x INTEGER NOT NULL, y INTEGER NOT NULL, c INTEGER NOT NULL,
	ip TEXT NOT NULL, cookie TEXT NOT NULL, ts BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS paint_log_ip ON paint_log (ip);
CREATE TABLE IF NOT EXISTS widgets (
	id BIGSERIAL PRIMARY KEY, kind TEXT NOT NULL, x INTEGER NOT NULL, y INTEGER NOT NULL,
	w INTEGER NOT NULL, h INTEGER NOT NULL, z INTEGER NOT NULL, color INTEGER NOT NULL,
	fill INTEGER NOT NULL, border INTEGER NOT NULL, text TEXT NOT NULL, font_size INTEGER NOT NULL,
	font TEXT NOT NULL, align TEXT NOT NULL, grey BOOLEAN NOT NULL, image_src BYTEA, updated_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS bans (ip TEXT PRIMARY KEY, ts BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value BIGINT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO canvas (id, data) VALUES (1, $1) ON CONFLICT DO NOTHING`, make([]byte, GridW*GridH)); err != nil {
		db.Close()
		return nil, err
	}
	if sqlitePath != "" {
		if err := importSQLite(db, sqlitePath); err != nil {
			db.Close()
			return nil, fmt.Errorf("import %s: %w", sqlitePath, err)
		}
	}
	s := &Store{db: db, widgets: map[int64]*Widget{}, epoch: time.Now().Unix()}
	if err := db.QueryRow(`SELECT data FROM canvas WHERE id = 1`).Scan(&s.canvas); err != nil {
		return nil, err
	}
	if len(s.canvas) != GridW*GridH {
		return nil, fmt.Errorf("canvas blob is %d bytes, want %d", len(s.canvas), GridW*GridH)
	}
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'cleared_after'`).Scan(&s.clearedAfter); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	s.bans = map[string]bool{}
	bans, err := db.Query(`SELECT ip FROM bans`)
	if err != nil {
		return nil, err
	}
	for bans.Next() {
		var ip string
		if err := bans.Scan(&ip); err != nil {
			return nil, err
		}
		s.bans[ip] = true
	}
	if err := bans.Close(); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, kind, x, y, w, h, z, color, fill, border, text, font_size, font, align, grey, image_src FROM widgets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		w := &Widget{}
		if err := rows.Scan(&w.ID, &w.Kind, &w.X, &w.Y, &w.W, &w.H, &w.Z, &w.Color, &w.Fill, &w.Border, &w.Text, &w.FontSize, &w.Font, &w.Align, &w.Grey, &w.src); err != nil {
			return nil, err
		}
		if err := w.redither(); err != nil {
			return nil, fmt.Errorf("widget %d: %w", w.ID, err)
		}
		s.widgets[w.ID] = w
	}
	return s, rows.Err()
}

func (s *Store) Close() error { return s.db.Close() }

// ETag changes on every edit and across restarts.
func (s *Store) ETag() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf(`"%d-%d"`, s.epoch, s.version)
}

func (s *Store) Canvas() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.canvas...)
}

// Paint sets each cell to colorAt(x, y) in one transaction and returns the cells
// that changed, by colour. Cells must be in range.
func (s *Store) Paint(cells [][2]int, colorAt func(x, y int) uint8, ip, cookie string) (map[uint8][][2]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]byte(nil), s.canvas...)
	changed := map[uint8][][2]int{}
	n := 0
	for _, p := range cells {
		c := colorAt(p[0], p[1])
		if i := p[1]*GridW + p[0]; next[i] != c {
			next[i] = c
			changed[c] = append(changed[c], p)
			n++
		}
	}
	if n == 0 {
		return changed, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE canvas SET data = $1 WHERE id = 1`, next); err != nil {
		return nil, err
	}
	xs, ys, cs := make([]int32, 0, n), make([]int32, 0, n), make([]int32, 0, n)
	for c, ps := range changed {
		for _, p := range ps {
			xs, ys, cs = append(xs, int32(p[0])), append(ys, int32(p[1])), append(cs, int32(c))
		}
	}
	if _, err := tx.Exec(`INSERT INTO paint_log (x, y, c, ip, cookie, ts)
		SELECT x, y, c, $4, $5, $6 FROM unnest($1::int[], $2::int[], $3::int[]) AS t(x, y, c)`,
		xs, ys, cs, ip, cookie, time.Now().Unix()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.canvas = next
	s.version++
	return changed, nil
}

func (s *Store) ClearCanvas() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	blank := make([]byte, GridW*GridH)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM paint_log`).Scan(&last); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE canvas SET data = $1 WHERE id = 1`, blank); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('cleared_after', $1)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, last); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.canvas, s.clearedAfter = blank, last
	s.version++
	return nil
}

func (s *Store) Widgets() []Widget {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.widgetsLocked()
}

func (s *Store) widgetsLocked() []Widget {
	out := make([]Widget, 0, len(s.widgets))
	for _, w := range s.widgets {
		out = append(out, *w)
	}
	return out
}

func NewWidget(kind string) Widget {
	w := Widget{Kind: kind, X: 40, Y: 40, W: 240, H: 120, Color: RGBBlack, Fill: -1, FontSize: 32, Font: "mono", Align: "left"}
	switch kind {
	case KindText:
		w.Text = "hello"
		w.Fill = RGBWhite
	case KindBox:
		w.Border = 4
	}
	return w
}

func (p WidgetPatch) apply(w *Widget) {
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if p.Kind != nil {
		w.Kind = *p.Kind
	}
	set(&w.X, p.X)
	set(&w.Y, p.Y)
	set(&w.W, p.W)
	set(&w.H, p.H)
	set(&w.Z, p.Z)
	set(&w.Fill, p.Fill)
	set(&w.Border, p.Border)
	set(&w.FontSize, p.FontSize)
	set(&w.Color, p.Color)
	if p.Text != nil {
		w.Text = *p.Text
	}
	if p.Align != nil {
		w.Align = *p.Align
	}
	if p.Font != nil {
		w.Font = *p.Font
	}
	if p.Grey != nil {
		w.Grey = *p.Grey
	}
}

func (w *Widget) validate() error {
	switch {
	case w.Kind != KindText && w.Kind != KindBox && w.Kind != KindImage:
		return errors.New("kind must be text, box or image")
	case w.W < 1 || w.W > PanelW || w.H < 1 || w.H > PanelH:
		return fmt.Errorf("size must be within %dx%d", PanelW, PanelH)
	case w.X <= -w.W || w.X >= PanelW || w.Y <= -w.H || w.Y >= PanelH:
		return errors.New("widget is entirely off screen")
	case w.Color < 0 || w.Color > RGBWhite:
		return errors.New("color must be 0xRRGGBB")
	case w.Fill < -1 || w.Fill > RGBWhite:
		return errors.New("fill must be -1 (none) or 0xRRGGBB")
	case w.Border < 0 || w.Border > 64:
		return errors.New("border must be 0-64")
	case w.FontSize < 8 || w.FontSize > 300:
		return errors.New("font_size must be 8-300")
	case len(w.Text) > 2000:
		return errors.New("text too long")
	case w.Align != "left" && w.Align != "center":
		return errors.New("align must be left or center")
	case Fonts[w.Font] == nil:
		return errors.New("unknown font")
	}
	return nil
}

func (w *Widget) redither() error {
	w.HasImage = len(w.src) > 0
	w.dither = nil
	if !w.HasImage {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(w.src))
	if err != nil {
		return err
	}
	w.dither = Dither(img, w.W, w.H, w.Grey)
	return nil
}

// CheckImage rejects files that are not decodable images or would decode too large.
func CheckImage(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("not a png, jpeg, gif or webp image: %w", err)
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return fmt.Errorf("image is %dx%d, max %d pixels", cfg.Width, cfg.Height, maxImagePixels)
	}
	return nil
}

func (s *Store) CreateWidget(w Widget) (Widget, error) {
	if err := w.validate(); err != nil {
		return Widget{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRow(`INSERT INTO widgets (kind, x, y, w, h, z, color, fill, border, text, font_size, font, align, grey, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
		w.Kind, w.X, w.Y, w.W, w.H, w.Z, w.Color, w.Fill, w.Border, w.Text, w.FontSize, w.Font, w.Align, w.Grey, time.Now().Unix(),
	).Scan(&w.ID); err != nil {
		return Widget{}, err
	}
	s.widgets[w.ID] = &w
	s.version++
	s.widgetVersion++
	return w, nil
}

// UpdateWidget applies a patch, and replaces the image when src is non-nil.
func (s *Store) UpdateWidget(id int64, p WidgetPatch, src []byte) (Widget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.widgets[id]
	if !ok {
		return Widget{}, ErrNotFound
	}
	next := *cur
	p.apply(&next)
	if err := next.validate(); err != nil {
		return Widget{}, err
	}
	if src != nil {
		next.src = src
	}
	if src != nil || next.W != cur.W || next.H != cur.H || next.Grey != cur.Grey {
		if err := next.redither(); err != nil {
			return Widget{}, err
		}
	}
	_, err := s.db.Exec(`UPDATE widgets SET kind = $1, x = $2, y = $3, w = $4, h = $5, z = $6, color = $7, fill = $8, border = $9,
		text = $10, font_size = $11, font = $12, align = $13, grey = $14, image_src = $15, updated_at = $16 WHERE id = $17`,
		next.Kind, next.X, next.Y, next.W, next.H, next.Z, next.Color, next.Fill, next.Border,
		next.Text, next.FontSize, next.Font, next.Align, next.Grey, next.src, time.Now().Unix(), id)
	if err != nil {
		return Widget{}, err
	}
	s.widgets[id] = &next
	s.version++
	s.widgetVersion++
	return next, nil
}

func (s *Store) DeleteWidget(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.widgets[id]; !ok {
		return ErrNotFound
	}
	if _, err := s.db.Exec(`DELETE FROM widgets WHERE id = $1`, id); err != nil {
		return err
	}
	delete(s.widgets, id)
	s.version++
	s.widgetVersion++
	return nil
}

// Frame returns the packed badge frame, its rendered image and its ETag, rendering at most once per version.
func (s *Store) Frame() ([]byte, *image.Paletted, string) {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	s.mu.RLock()
	version, etag := s.version, fmt.Sprintf(`"%d-%d"`, s.epoch, s.version)
	if s.frame != nil && s.frameVersion == version {
		s.mu.RUnlock()
		return s.frame, s.frameImg, etag
	}
	canvas := append([]byte(nil), s.canvas...)
	widgets := s.widgetsLocked()
	s.mu.RUnlock()
	img := Render(canvas, widgets)
	s.frame, s.frameImg, s.frameVersion = PackFrame(img), img, version
	return s.frame, s.frameImg, etag
}

func (s *Store) WidgetVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("%d-%d", s.epoch, s.widgetVersion)
}

// WidgetLayer is the widgets on a transparent background, for the web canvas.
func (s *Store) WidgetLayer() *image.Paletted {
	s.frameMu.Lock()
	defer s.frameMu.Unlock()
	s.mu.RLock()
	version := s.widgetVersion
	if s.layer != nil && s.layerVersion == version {
		s.mu.RUnlock()
		return s.layer
	}
	widgets := s.widgetsLocked()
	s.mu.RUnlock()
	s.layer, s.layerVersion = Render(nil, widgets), version
	return s.layer
}
