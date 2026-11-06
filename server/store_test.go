package main

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testDB returns a URL for an empty schema in TEST_DATABASE_URL, dropped after the test.
func testDB(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPaintRGBDithersPerCell(t *testing.T) {
	s := openTestStore(t)
	h := (&Server{store: s, hub: NewHub(), limiter: NewLimiter(0)}).Routes()
	req := httptest.NewRequest("POST", "/api/pixel", strings.NewReader(`{"p":[[0,0],[1,0],[0,1],[1,1]],"rgb":8421504}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	c := s.Canvas()
	got := []uint8{c[0], c[1], c[GridW], c[GridW+1]}
	for i, p := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
		if want := Ink(0x808080, p[0], p[1]); got[i] != want {
			t.Fatalf("cell %v = %d, want %d", p, got[i], want)
		}
	}
	if got[0] == got[1] {
		t.Fatalf("grey painted as one solid colour: %v", got)
	}
}

func TestPaintReturnsChangedCells(t *testing.T) {
	s := openTestStore(t)
	changed, err := s.Paint([][2]int{{0, 0}, {5, 3}}, solid(Black), "ip", "c")
	if err != nil || len(changed[Black]) != 2 {
		t.Fatalf("first paint: changed %v, err %v", changed, err)
	}
	changed, err = s.Paint([][2]int{{5, 3}, {6, 3}}, solid(Black), "ip", "c")
	if err != nil || len(changed[Black]) != 1 || changed[Black][0] != [2]int{6, 3} {
		t.Fatalf("second paint: changed %v, err %v", changed, err)
	}
	if got := s.Canvas()[3*GridW+5]; got != Black {
		t.Fatalf("cell (5,3) = %d, want black", got)
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	dbURL := testDB(t)
	s, err := OpenStore(dbURL, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Paint([][2]int{{7, 7}}, solid(Red), "ip", "c")
	w, err := s.CreateWidget(NewWidget(KindBox))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(dbURL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Canvas()[7*GridW+7] != Red {
		t.Fatal("paint lost on reopen")
	}
	if ws := s.Widgets(); len(ws) != 1 || ws[0].ID != w.ID || ws[0].Fill != w.Fill {
		t.Fatalf("widgets %+v, want %+v", ws, w)
	}
}

func TestImportSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ink.db")
	src, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	canvas := make([]byte, GridW*GridH)
	canvas[GridW+2], canvas[GridW+3] = Red, Black
	for _, q := range []string{
		`CREATE TABLE canvas (id INTEGER PRIMARY KEY, data BLOB NOT NULL)`,
		`CREATE TABLE paint_log (id INTEGER PRIMARY KEY, x INTEGER, y INTEGER, c INTEGER, ip TEXT, cookie TEXT, ts INTEGER)`,
		`CREATE TABLE widgets (id INTEGER PRIMARY KEY, kind TEXT, x INTEGER, y INTEGER, w INTEGER, h INTEGER, z INTEGER,
			color INTEGER, fill INTEGER, border INTEGER, text TEXT, font_size INTEGER, align TEXT, image_src BLOB,
			updated_at INTEGER, font TEXT, grey INTEGER)`,
		`CREATE TABLE bans (ip TEXT PRIMARY KEY, ts INTEGER)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value INTEGER)`,
		`INSERT INTO paint_log VALUES (5, 2, 1, 2, 'a', 'x', 1), (9, 3, 1, 1, 'b', 'y', 2)`,
		`INSERT INTO widgets VALUES (4, 'text', 0, 0, 100, 40, 1, 16711680, 16777215, 0, 'hi', 32, 'left', NULL, 1, 'mono', 1)`,
		`INSERT INTO bans VALUES ('b', 3)`,
		`INSERT INTO meta VALUES ('cleared_after', 5)`,
		`PRAGMA user_version = 3`,
	} {
		if _, err := src.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := src.Exec(`INSERT INTO canvas VALUES (1, ?)`, canvas); err != nil {
		t.Fatal(err)
	}
	src.Close()

	dbURL := testDB(t)
	s, err := OpenStore(dbURL, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Canvas()[GridW+2] != Red {
		t.Fatal("canvas not imported")
	}
	ws := s.Widgets()
	if len(ws) != 1 || ws[0].ID != 4 || ws[0].Color != RGBRed || !ws[0].Grey || ws[0].Text != "hi" {
		t.Fatalf("widgets %+v", ws)
	}
	if !s.Blocked("b") || s.clearedAfter != 5 {
		t.Fatalf("blocked %v, cleared_after %d", s.Blocked("b"), s.clearedAfter)
	}
	if cells, _ := s.PainterCells("b"); len(cells) != 1 || cells[0] != [2]int{3, 1} {
		t.Fatalf("b shows %v, want (3,1) only", cells)
	}
	w, err := s.CreateWidget(NewWidget(KindBox))
	if err != nil || w.ID != 5 {
		t.Fatalf("next widget id %d, err %v", w.ID, err)
	}
	s.Paint([][2]int{{2, 1}}, solid(Black), "c", "z")
	for _, ip := range []string{"a", "b", "c"} {
		s.RemovePainter(ip)
	}
	for _, w := range s.Widgets() {
		s.DeleteWidget(w.ID)
	}
	s.ClearCanvas()
	s.Close()

	s, err = OpenStore(dbURL, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Canvas()[GridW+2] != White || len(s.Widgets()) != 0 {
		t.Fatal("emptied database imported again on reopen")
	}
}
