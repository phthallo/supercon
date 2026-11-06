package main

import (
	"database/sql"
	"sort"
	"time"
)

// Paints logged before this time (the deploy that halved the cells) use the 120×84 grid.
const gridChangeTS = 1793999748

// migrateLog rewrites old-grid paint_log rows as 2×2 blocks, keeping log order.
func migrateLog(db *sql.DB) error {
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil || ver >= 1 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []struct {
		q    string
		args []any
	}{
		{`CREATE TABLE paint_log_new (
			id INTEGER PRIMARY KEY, x INTEGER NOT NULL, y INTEGER NOT NULL, c INTEGER NOT NULL,
			ip TEXT NOT NULL, cookie TEXT NOT NULL, ts INTEGER NOT NULL)`, nil},
		{`INSERT INTO paint_log_new (x, y, c, ip, cookie, ts)
			SELECT p.x*2 + d.dx, p.y*2 + d.dy, p.c, p.ip, p.cookie, p.ts
			FROM paint_log p, (SELECT 0 AS dx, 0 AS dy UNION ALL SELECT 1, 0 UNION ALL SELECT 0, 1 UNION ALL SELECT 1, 1) d
			WHERE p.ts < ? ORDER BY p.id, d.dy, d.dx`, []any{gridChangeTS}},
		{`INSERT INTO paint_log_new (x, y, c, ip, cookie, ts)
			SELECT x, y, c, ip, cookie, ts FROM paint_log WHERE ts >= ? ORDER BY id`, []any{gridChangeTS}},
		{`DROP TABLE paint_log`, nil},
		{`ALTER TABLE paint_log_new RENAME TO paint_log`, nil},
		{`PRAGMA user_version = 1`, nil},
	}
	for _, st := range steps {
		if _, err := tx.Exec(st.q, st.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type Painter struct {
	IP       string `json:"ip"`
	Showing  int    `json:"showing"`
	Painted  int    `json:"painted"`
	Browsers int    `json:"browsers"`
	Last     int64  `json:"last"`
	Blocked  bool   `json:"blocked"`
}

// scanLog replays paint_log since the last clear. owner[i] is the IP whose paint
// cell i shows, or "". prev[i] is the colour cell i would show without skip's
// paint, or -1 when nobody else painted it. Callers hold s.mu.
func (s *Store) scanLog(skip string) (owner []string, prev []int16, err error) {
	n := GridW * GridH
	owner, prev, last := make([]string, n), make([]int16, n), make([]int16, n)
	for i := range prev {
		prev[i], last[i] = -1, -1
	}
	rows, err := s.db.Query(`SELECT x, y, c, ip FROM paint_log WHERE id > ? ORDER BY id`, s.clearedAfter)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x, y int
		var c int16
		var ip string
		if err := rows.Scan(&x, &y, &c, &ip); err != nil {
			return nil, nil, err
		}
		if x < 0 || x >= GridW || y < 0 || y >= GridH {
			continue
		}
		i := y*GridW + x
		owner[i], last[i] = ip, c
		if ip != skip {
			prev[i] = c
		}
	}
	for i := range owner {
		if last[i] != int16(s.canvas[i]) {
			owner[i] = ""
		}
	}
	return owner, prev, rows.Err()
}

// Painters lists every IP in the paint log or the ban list, most visible first.
func (s *Store) Painters() ([]Painter, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, _, err := s.scanLog("")
	if err != nil {
		return nil, err
	}
	showing := map[string]int{}
	for _, ip := range owner {
		if ip != "" {
			showing[ip]++
		}
	}
	rows, err := s.db.Query(`SELECT ip, COUNT(*), COUNT(DISTINCT cookie), MAX(ts) FROM paint_log GROUP BY ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Painter{}
	seen := map[string]bool{}
	for rows.Next() {
		var p Painter
		if err := rows.Scan(&p.IP, &p.Painted, &p.Browsers, &p.Last); err != nil {
			return nil, err
		}
		p.Showing, p.Blocked, seen[p.IP] = showing[p.IP], s.bans[p.IP], true
		out = append(out, p)
	}
	for ip := range s.bans {
		if !seen[ip] {
			out = append(out, Painter{IP: ip, Blocked: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Showing != out[j].Showing {
			return out[i].Showing > out[j].Showing
		}
		return out[i].Last > out[j].Last
	})
	return out, rows.Err()
}

// PainterCells returns the cells currently showing ip's paint.
func (s *Store) PainterCells(ip string) ([][2]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner, _, err := s.scanLog("")
	if err != nil {
		return nil, err
	}
	cells := [][2]int{}
	for i, o := range owner {
		if o == ip {
			cells = append(cells, [2]int{i % GridW, i / GridW})
		}
	}
	return cells, nil
}

// RemovePainter deletes ip's paint log and puts each cell it was showing back to
// what the next most recent painter left there. It returns the changed cells by colour.
func (s *Store) RemovePainter(ip string) (map[uint8][][2]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, prev, err := s.scanLog(ip)
	if err != nil {
		return nil, err
	}
	next := append([]byte(nil), s.canvas...)
	changed := map[uint8][][2]int{}
	for i, o := range owner {
		if o != ip {
			continue
		}
		v := White
		if prev[i] >= 0 {
			v = uint8(prev[i])
		}
		if next[i] != v {
			next[i] = v
			changed[v] = append(changed[v], [2]int{i % GridW, i / GridW})
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM paint_log WHERE ip = ?`, ip); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE canvas SET data = ? WHERE id = 1`, next); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		s.canvas = next
		s.version++
	}
	return changed, nil
}

func (s *Store) SetBlocked(ip string, blocked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if blocked {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO bans (ip, ts) VALUES (?, ?)`, ip, time.Now().Unix())
	} else {
		_, err = s.db.Exec(`DELETE FROM bans WHERE ip = ?`, ip)
	}
	if err != nil {
		return err
	}
	if blocked {
		s.bans[ip] = true
	} else {
		delete(s.bans, ip)
	}
	return nil
}

func (s *Store) Blocked(ip string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bans[ip]
}
