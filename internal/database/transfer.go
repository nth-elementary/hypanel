package database

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

// tempImportPrefix marks uploads made just for an import. They're removed afterwards; any
// other server path the user names is left alone.
const tempImportPrefix = "/tmp/hypanel-import-"

const csvNull = `\N` // how mysql's own tools write NULL, since CSV has no way to say it

// export streams a dump of a database or some of its tables, as SQL.
func (d *Database) export(w http.ResponseWriter, r *http.Request) {
	inst, db := chi.URLParam(r, "inst"), chi.URLParam(r, "db")
	if !validCont.MatchString(inst) || !validIdent.MatchString(db) {
		core.Fail(w, http.StatusBadRequest, "invalid name")
		return
	}
	q := r.URL.Query()
	args := []string{"--single-transaction", "--quick", "--hex-blob", "--default-character-set=utf8mb4"}
	switch q.Get("content") {
	case "structure":
		args = append(args, "--no-data")
	case "data":
		args = append(args, "--no-create-info")
	}
	name := db
	if list := q.Get("tables"); list != "" {
		args = append(args, db)
		tables := strings.Split(list, ",")
		for _, t := range tables {
			// The name check also stops one from being read as a command-line option.
			if !validNewName.MatchString(t) || strings.HasPrefix(t, "-") {
				core.Fail(w, http.StatusBadRequest, "invalid table name %q", t)
				return
			}
		}
		args = append(args, tables...)
		if len(tables) == 1 {
			name = db + "-" + tables[0]
		}
	} else {
		args = append(args, "--routines", "--databases", db)
	}
	script := `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; exec "$(command -v mariadb-dump || command -v mysqldump)" -uroot "$@"`
	cmd := exec.CommandContext(r.Context(), "docker", append([]string{"exec", inst, "sh", "-c", script, "sh"}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "db.export", name)

	stamp := time.Now().Format("20060102-150405")
	var out io.Writer = w
	var gz *gzip.Writer
	if q.Get("gzip") == "0" {
		w.Header().Set("Content-Type", "application/sql")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.sql"`, name, stamp))
	} else {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.sql.gz"`, name, stamp))
		gz = gzip.NewWriter(w)
		out = gz
	}
	_, copyErr := io.Copy(out, stdout)
	waitErr := cmd.Wait()
	// Aborting makes the browser report a failed download, rather than saving a truncated
	// dump that looks complete and fails, or worse half-restores, later.
	if copyErr != nil || waitErr != nil {
		panic(http.ErrAbortHandler)
	}
	if gz != nil {
		_ = gz.Close()
	}
}

// exportCSV streams one table, or the rows matching a search.
func (d *Database) exportCSV(w http.ResponseWriter, r *http.Request) {
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	db, table := chi.URLParam(r, "db"), chi.URLParam(r, "table")
	cols, err := tableColumns(ctx, conn, db, table)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	where, args, err := searchClause(cols, r.URL.Query().Get("search"), r.URL.Query().Get("column"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	rows, err := conn.QueryContext(ctx, "SELECT * FROM "+quote(table)+where, args...)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "db.export.csv", db+"."+table)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, table, time.Now().Format("20060102-150405")))
	// The byte-order mark is how Excel knows it's UTF-8; without it Chinese text is garbled.
	_, _ = w.Write([]byte("\xef\xbb\xbf"))
	out := csv.NewWriter(w)
	_ = out.Write(names)
	raw := make([][]byte, len(names))
	ptrs := make([]any, len(names))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	record := make([]string, len(names))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			panic(http.ErrAbortHandler)
		}
		for i, v := range raw {
			if v == nil {
				record[i] = csvNull
			} else {
				record[i] = string(v)
			}
		}
		_ = out.Write(record)
	}
	out.Flush()
	if rows.Err() != nil || out.Error() != nil {
		panic(http.ErrAbortHandler)
	}
}

func cleanupImport(path string) {
	if strings.HasPrefix(path, tempImportPrefix) {
		_ = os.Remove(path)
	}
}

// importDump runs a .sql or .sql.gz from the server into a database, as a background task
// so a large file isn't tied to one long request. The browser uploads it in chunks first,
// which also gets it past Cloudflare's limit on request size.
func (d *Database) importDump(w http.ResponseWriter, r *http.Request) {
	inst, db := chi.URLParam(r, "inst"), chi.URLParam(r, "db")
	if !validCont.MatchString(inst) || !validIdent.MatchString(db) || systemDBs[db] {
		core.Fail(w, http.StatusBadRequest, "invalid name")
		return
	}
	var req struct{ Path string }
	if !core.Read(w, r, &req) {
		return
	}
	if !strings.HasPrefix(req.Path, "/") {
		core.Fail(w, http.StatusBadRequest, "path must be absolute")
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.import", db+" from "+req.Path)
	task := d.app.Tasks.Start("Import into "+db, func(ctx context.Context, t *core.Task) error {
		defer cleanupImport(req.Path)
		in, err := os.Open(req.Path)
		if err != nil {
			return err
		}
		defer in.Close()
		body := bufio.NewReader(in)
		var src io.Reader = body
		if head, _ := body.Peek(2); len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
			gz, err := gzip.NewReader(body)
			if err != nil {
				return err
			}
			defer gz.Close()
			src = gz
		}
		script := `export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-$MARIADB_ROOT_PASSWORD}"; exec "$(command -v mariadb || command -v mysql)" -uroot --default-character-set=utf8mb4 "$1"`
		cmd := exec.CommandContext(ctx, "docker", "exec", "-i", inst, "sh", "-c", script, "sh", db)
		cmd.Stdin = src
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		start := time.Now()
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(out.String())
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("%s", msg)
		}
		t.Printf("Imported in %s\n", time.Since(start).Round(time.Millisecond))
		return nil
	})
	core.OK(w, map[string]string{"task": task.ID})
}

const csvBatch = 200

// importCSV loads a CSV whose first row names the columns. It all happens in one
// transaction where the engine supports that, so a bad row leaves InnoDB tables unchanged.
func (d *Database) importCSV(w http.ResponseWriter, r *http.Request) {
	if !writableDB(w, r) {
		return
	}
	var req struct{ Path, Mode string }
	if !core.Read(w, r, &req) {
		return
	}
	verb, valid := map[string]string{"": "INSERT", "insert": "INSERT", "ignore": "INSERT IGNORE", "replace": "REPLACE"}[req.Mode]
	if !valid || !strings.HasPrefix(req.Path, "/") {
		core.Fail(w, http.StatusBadRequest, "invalid mode or path")
		return
	}
	inst, db, table := chi.URLParam(r, "inst"), chi.URLParam(r, "db"), chi.URLParam(r, "table")
	d.app.Audit(r, "db.import.csv", db+"."+table)
	task := d.app.Tasks.Start("Import CSV into "+table, func(ctx context.Context, t *core.Task) error {
		defer cleanupImport(req.Path)
		conn, err := open(ctx, inst, db)
		if err != nil {
			return err
		}
		defer conn.Close()
		cols, err := tableColumns(ctx, conn, db, table)
		if err != nil {
			return err
		}
		in, err := os.Open(req.Path)
		if err != nil {
			return err
		}
		defer in.Close()
		body := bufio.NewReader(in)
		if bom, _ := body.Peek(3); bytes.Equal(bom, []byte("\xef\xbb\xbf")) {
			_, _ = body.Discard(3)
		}
		reader := csv.NewReader(body)
		header, err := reader.Read()
		if err != nil {
			return fmt.Errorf("can't read the header row: %w", err)
		}
		names := make([]string, len(header))
		marks := make([]string, len(header))
		for i, h := range header {
			h = strings.TrimSpace(h)
			if !knownColumn(cols, h) {
				return fmt.Errorf("the file has a column this table doesn't: %q", h)
			}
			names[i], marks[i] = quote(h), "?"
		}
		stmt := verb + " INTO " + quote(table) + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // does nothing once committed
		line, affected := 1, int64(0)
		args := make([]any, len(header))
		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			line++
			if err != nil {
				return fmt.Errorf("line %d: %w", line, err)
			}
			for i, v := range record {
				if v == csvNull {
					args[i] = nil
				} else {
					args[i] = v
				}
			}
			res, err := tx.ExecContext(ctx, stmt, args...)
			if err != nil {
				return fmt.Errorf("line %d: %w. Nothing was imported", line, err)
			}
			n, _ := res.RowsAffected()
			affected += n
			if line%csvBatch == 0 {
				t.Printf("%d rows read\n", line-1)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		t.Printf("Done: %d rows read, %d changed\n", line-1, affected)
		return nil
	})
	core.OK(w, map[string]string{"task": task.ID})
}
