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
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
	_ "modernc.org/sqlite"
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
	Color    uint8  `json:"color"`
	Fill     int    `json:"fill"`
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
	Color    *uint8  `json:"color"`
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

	frameMu      sync.Mutex
	frameVersion int64
	frame        []byte
	frameImg     *image.Paletted
	layerVersion int64
	layer        *image.Paletted
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS canvas (id INTEGER PRIMARY KEY CHECK (id = 1), data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS paint_log (
	id INTEGER PRIMARY KEY, x INTEGER NOT NULL, y INTEGER NOT NULL, c INTEGER NOT NULL,
	ip TEXT NOT NULL, cookie TEXT NOT NULL, ts INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS widgets (
	id INTEGER PRIMARY KEY, kind TEXT NOT NULL, x INTEGER NOT NULL, y INTEGER NOT NULL,
	w INTEGER NOT NULL, h INTEGER NOT NULL, z INTEGER NOT NULL, color INTEGER NOT NULL,
	fill INTEGER NOT NULL, border INTEGER NOT NULL, text TEXT NOT NULL, font_size INTEGER NOT NULL,
	align TEXT NOT NULL DEFAULT 'left', image_src BLOB, updated_at INTEGER NOT NULL,
	font TEXT NOT NULL DEFAULT 'mono', grey INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		return nil, err
	}
	// Databases created before these columns existed.
	for _, col := range []string{`font TEXT NOT NULL DEFAULT 'mono'`, `grey INTEGER NOT NULL DEFAULT 0`} {
		if _, err := db.Exec(`ALTER TABLE widgets ADD COLUMN ` + col); err != nil &&
			!strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	s := &Store{db: db, widgets: map[int64]*Widget{}, epoch: time.Now().Unix()}
	if err := db.QueryRow(`SELECT data FROM canvas WHERE id = 1`).Scan(&s.canvas); errors.Is(err, sql.ErrNoRows) {
		s.canvas = make([]byte, GridW*GridH)
		if _, err := db.Exec(`INSERT INTO canvas (id, data) VALUES (1, ?)`, s.canvas); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// The grid was 120×84 before the cells were halved to 4 panel pixels.
	if oldW, oldH := 120, 84; len(s.canvas) == oldW*oldH && oldW*2 == GridW && oldH*2 == GridH {
		up := make([]byte, GridW*GridH)
		for i := range up {
			x, y := i%GridW, i/GridW
			up[i] = s.canvas[(y/2)*oldW+x/2]
		}
		if _, err := db.Exec(`UPDATE canvas SET data = ? WHERE id = 1`, up); err != nil {
			return nil, err
		}
		s.canvas = up
	}
	if len(s.canvas) != GridW*GridH {
		return nil, fmt.Errorf("canvas blob is %d bytes, want %d", len(s.canvas), GridW*GridH)
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

// Paint sets every cell in cells to c in one transaction and returns the cells that changed.
// Cells must be in range.
func (s *Store) Paint(cells [][2]int, c uint8, ip, cookie string) ([][2]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := append([]byte(nil), s.canvas...)
	changed := make([][2]int, 0, len(cells))
	for _, p := range cells {
		if i := p[1]*GridW + p[0]; next[i] != c {
			next[i] = c
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return changed, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE canvas SET data = ? WHERE id = 1`, next); err != nil {
		return nil, err
	}
	stmt, err := tx.Prepare(`INSERT INTO paint_log (x, y, c, ip, cookie, ts) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, p := range changed {
		if _, err := stmt.Exec(p[0], p[1], c, ip, cookie, now); err != nil {
			return nil, err
		}
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
	if _, err := s.db.Exec(`UPDATE canvas SET data = ? WHERE id = 1`, blank); err != nil {
		return err
	}
	s.canvas = blank
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
	w := Widget{Kind: kind, X: 40, Y: 40, W: 240, H: 120, Color: Black, Fill: -1, FontSize: 32, Font: "mono", Align: "left"}
	switch kind {
	case KindText:
		w.Text = "hello"
		w.Fill = int(White)
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
	if p.Color != nil {
		w.Color = *p.Color
	}
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
	case w.Color > Red:
		return errors.New("color must be 0 (white), 1 (black) or 2 (red)")
	case w.Fill < -1 || w.Fill > int(Red):
		return errors.New("fill must be -1 (none) or a color")
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
	res, err := s.db.Exec(`INSERT INTO widgets (kind, x, y, w, h, z, color, fill, border, text, font_size, font, align, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.Kind, w.X, w.Y, w.W, w.H, w.Z, w.Color, w.Fill, w.Border, w.Text, w.FontSize, w.Font, w.Align, time.Now().Unix())
	if err != nil {
		return Widget{}, err
	}
	w.ID, _ = res.LastInsertId()
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
	_, err := s.db.Exec(`UPDATE widgets SET kind = ?, x = ?, y = ?, w = ?, h = ?, z = ?, color = ?, fill = ?, border = ?,
		text = ?, font_size = ?, font = ?, align = ?, grey = ?, image_src = ?, updated_at = ? WHERE id = ?`,
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
	if _, err := s.db.Exec(`DELETE FROM widgets WHERE id = ?`, id); err != nil {
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
