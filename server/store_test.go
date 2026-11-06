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
