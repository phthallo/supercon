package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// importSQLite copies the ink.db of the SQLite version of this server into dst.
// It runs at most once, and never into a database that already has paint or widgets.
func importSQLite(dst *sql.DB, path string) error {
	var used bool
	if err := dst.QueryRow(`SELECT EXISTS (SELECT 1 FROM meta WHERE key = 'sqlite_imported')
		OR EXISTS (SELECT 1 FROM paint_log) OR EXISTS (SELECT 1 FROM widgets)`).Scan(&used); err != nil || used {
		return err
	}
	src, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()
	var ver int
	if err := src.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	// Version 3 has the 240×168 paint log and 0xRRGGBB widget colours.
	if ver < 3 {
		return fmt.Errorf("schema version %d; run commit 9e45110 on it once to upgrade it first", ver)
	}
	var canvas []byte
	if err := src.QueryRow(`SELECT data FROM canvas WHERE id = 1`).Scan(&canvas); err != nil {
		return err
	}
	if len(canvas) != GridW*GridH {
		return fmt.Errorf("canvas blob is %d bytes, want %d", len(canvas), GridW*GridH)
	}

	tx, err := dst.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE canvas SET data = $1 WHERE id = 1`, canvas); err != nil {
		return err
	}
	tables := []struct{ name, cols, vals string }{
		{"paint_log", "id, x, y, c, ip, cookie, ts", ""},
		{"widgets", "id, kind, x, y, w, h, z, color, fill, border, text, font_size, font, align, grey, image_src, updated_at",
			"$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::int <> 0, $16, $17"},
		{"bans", "ip, ts", ""},
		{"meta", "key, value", ""},
	}
	for _, t := range tables {
		n, err := copyRows(src, tx, t.name, t.cols, t.vals)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		log.Printf("import: %d rows into %s", n, t.name)
	}
	for _, t := range []string{"paint_log", "widgets"} {
		if _, err := tx.Exec(fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('%[1]s', 'id'), COALESCE((SELECT MAX(id) FROM %[1]s), 0) + 1, false)`, t)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('sqlite_imported', $1)`, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// copyRows copies cols of table from src to dst. vals is the VALUES list, or empty for $1, $2, ….
func copyRows(src *sql.DB, dst *sql.Tx, table, cols, vals string) (int, error) {
	rows, err := src.Query(fmt.Sprintf(`SELECT %s FROM %s ORDER BY 1`, cols, table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if vals == "" {
		for i := range names {
			if i > 0 {
				vals += ", "
			}
			vals += fmt.Sprintf("$%d", i+1)
		}
	}
	stmt, err := dst.Prepare(fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, cols, vals))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	row := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range row {
		ptrs[i] = &row[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		if _, err := stmt.Exec(row...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
