package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestOpenStoreConvertsPaletteWidgetColours(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ink.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO widgets (kind, x, y, w, h, z, color, fill, border, text, font_size, updated_at)
		VALUES ('box', 0, 0, 10, 10, 0, 2, 0, 1, '', 32, 0), ('box', 0, 0, 10, 10, 0, 0, -1, 1, '', 32, 0);
		PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := map[int64][2]int{}
	for _, w := range s.Widgets() {
		got[w.ID] = [2]int{w.Color, w.Fill}
	}
	if got[1] != [2]int{RGBRed, RGBWhite} || got[2] != [2]int{RGBWhite, -1} {
		t.Fatalf("colour, fill = %v", got)
	}
}

func TestPaintReturnsChangedCells(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ink.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

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

func TestOpenStoreUpscalesOldCanvas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ink.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	old := make([]byte, 120*84)
	old[1*120+2] = Red
	if _, err := s.db.Exec(`UPDATE canvas SET data = ? WHERE id = 1`, old); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := s.Canvas()
	for _, p := range [][2]int{{4, 2}, {5, 2}, {4, 3}, {5, 3}} {
		if c[p[1]*GridW+p[0]] != Red {
			t.Fatalf("cell %v not red after upscale", p)
		}
	}
	if c[2*GridW+6] != White || c[1*GridW+4] != White {
		t.Fatal("upscale spilled past the 2x2 block")
	}
}
