package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemovePainterRestoresWhatWasUnderneath(t *testing.T) {
	s := openTestStore(t)
	s.Paint([][2]int{{1, 1}, {2, 1}}, solid(Red), "good", "a")
	s.Paint([][2]int{{2, 1}, {3, 1}}, solid(Black), "bad", "b")
	s.Paint([][2]int{{3, 1}}, solid(Red), "good", "a")

	cells, _ := s.PainterCells("bad")
	if len(cells) != 1 || cells[0] != [2]int{2, 1} {
		t.Fatalf("bad shows %v, want only (2,1)", cells)
	}
	changed, err := s.RemovePainter("bad")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed[Red]) != 1 || changed[Red][0] != [2]int{2, 1} {
		t.Fatalf("changed %v", changed)
	}
	c := s.Canvas()
	if c[GridW+1] != Red || c[GridW+2] != Red || c[GridW+3] != Red {
		t.Fatalf("row 1 = %v, want red red red", c[GridW+1:GridW+4])
	}
	painters, _ := s.Painters()
	if len(painters) != 1 || painters[0].IP != "good" || painters[0].Showing != 3 {
		t.Fatalf("painters %+v", painters)
	}
}

func TestRemovePainterIgnoresPaintBeforeClear(t *testing.T) {
	s := openTestStore(t)
	s.Paint([][2]int{{0, 0}}, solid(Red), "good", "a")
	if err := s.ClearCanvas(); err != nil {
		t.Fatal(err)
	}
	s.Paint([][2]int{{0, 0}}, solid(Black), "bad", "b")
	if _, err := s.RemovePainter("bad"); err != nil {
		t.Fatal(err)
	}
	if got := s.Canvas()[0]; got != White {
		t.Fatalf("cell (0,0) = %d, want white", got)
	}
}

func TestBlockedIPCannotPaintButBadgeStillWorks(t *testing.T) {
	s := openTestStore(t)
	srv := &Server{store: s, hub: NewHub(), limiter: NewLimiter(0)}
	h := srv.Routes()
	if err := s.SetBlocked("192.0.2.1", true); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{"/": http.StatusForbidden, "/api/canvas": http.StatusForbidden, "/badge/frame": http.StatusOK, "/static/admin.js": http.StatusOK} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "192.0.2.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}
