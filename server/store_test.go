package main

import (
	"path/filepath"
	"testing"
)

func TestPaintReturnsChangedCells(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "ink.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	changed, err := s.Paint([][2]int{{0, 0}, {5, 3}}, Black, "ip", "c")
	if err != nil || len(changed) != 2 {
		t.Fatalf("first paint: changed %v, err %v", changed, err)
	}
	changed, err = s.Paint([][2]int{{5, 3}, {6, 3}}, Black, "ip", "c")
	if err != nil || len(changed) != 1 || changed[0] != [2]int{6, 3} {
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
	if _, err := s.db.Exec(`PRAGMA user_version = 0`); err != nil {
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
		if c[p[1]*GridW+p[0]] != Black {
			t.Fatalf("cell %v not upscaled and turned black", p)
		}
	}
	if c[2*GridW+6] != White || c[1*GridW+4] != White {
		t.Fatal("upscale spilled past the 2x2 block")
	}
}
