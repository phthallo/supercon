package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "ink.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRemovePainterRestoresWhatWasUnderneath(t *testing.T) {
	s := openTestStore(t)
	s.Paint([][2]int{{1, 1}, {2, 1}}, Red, "good", "a")
	s.Paint([][2]int{{2, 1}, {3, 1}}, Black, "bad", "b")
	s.Paint([][2]int{{3, 1}}, Red, "good", "a")

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
	s.Paint([][2]int{{0, 0}}, Red, "good", "a")
	if err := s.ClearCanvas(); err != nil {
		t.Fatal(err)
	}
	s.Paint([][2]int{{0, 0}}, Black, "bad", "b")
	if _, err := s.RemovePainter("bad"); err != nil {
		t.Fatal(err)
	}
	if got := s.Canvas()[0]; got != White {
		t.Fatalf("cell (0,0) = %d, want white", got)
	}
}

func TestMigrateLogExpandsOldGridRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ink.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO paint_log (x, y, c, ip, cookie, ts) VALUES (3, 2, 1, 'old', 'a', ?), (3, 2, 2, 'new', 'b', ?)`,
		gridChangeTS-1, gridChangeTS+1); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`SELECT x, y, ip FROM paint_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var x, y int
		var ip string
		rows.Scan(&x, &y, &ip)
		got = append(got, fmt.Sprintf("%s:%d,%d", ip, x, y))
	}
	want := []string{"old:6,4", "old:7,4", "old:6,5", "old:7,5", "new:3,2"}
	if len(got) != len(want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows %v, want %v", got, want)
		}
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
