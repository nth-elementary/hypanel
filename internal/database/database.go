// Package database is the phpMyAdmin replacement: MariaDB/MySQL containers are found
// automatically, then databases, users, tables and rows can be managed, SQL run, and
// dumps imported or exported.
package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/go-sql-driver/mysql"

	"hypanel/internal/core"
)

var (
	validIdent = regexp.MustCompile(`^[a-zA-Z0-9_$-]{1,64}$`)
	validHost  = regexp.MustCompile(`^[a-zA-Z0-9.%:_-]{1,255}$`)
	validCont  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

var systemDBs = map[string]bool{"information_schema": true, "performance_schema": true, "mysql": true, "sys": true}

const (
	maxRows     = 1000
	queryWindow = 60 * time.Second
)

type Database struct{ app *core.App }

func New(app *core.App) *Database { return &Database{app: app} }

func (d *Database) Name() string { return "database" }

func (d *Database) Routes(r chi.Router) {
	r.Get("/db/instances", d.instances)
	r.Route("/db/{inst}", func(r chi.Router) {
		r.Get("/databases", d.databases)
		r.Post("/databases", d.createDatabase)
		r.Delete("/databases/{db}", d.dropDatabase)
		r.Get("/databases/{db}/tables", d.tables)
		r.Get("/databases/{db}/tables/{table}/rows", d.rows)
		r.Get("/databases/{db}/tables/{table}/columns", d.columns)
		r.Get("/databases/{db}/tables/{table}/structure", d.structure)
		r.Post("/databases/{db}/tables", d.tableCreate)
		r.Post("/databases/{db}/tables/{table}/alter", d.tableAlter)
		r.Delete("/databases/{db}/tables/{table}", d.tableDrop)
		r.Post("/databases/{db}/tables/{table}/truncate", d.tableTruncate)
		r.Get("/databases/{db}/tables/{table}/export.csv", d.exportCSV)
		r.Post("/databases/{db}/tables/{table}/import-csv", d.importCSV)
		r.Post("/databases/{db}/search", d.search)
		r.Post("/databases/{db}/tables/{table}/rows", d.rowInsert)
		r.Put("/databases/{db}/tables/{table}/rows", d.rowUpdate)
		r.Post("/databases/{db}/tables/{table}/rows/delete", d.rowDelete)
		r.Post("/databases/{db}/query", d.query)
		r.Get("/databases/{db}/export", d.export)
		r.Post("/databases/{db}/import", d.importDump)
		r.Get("/users", d.users)
		r.Post("/users", d.userCreate)
		r.Post("/users/password", d.userPassword)
		r.Post("/users/grant", d.userGrant)
		r.Post("/users/drop", d.userDrop)
	})
}

type dockerContainer struct{ Name, Image string }

// findInstances lists running database containers. The root password comes from the
// container's own environment at connection time, so it's never stored by the panel.
func findInstances(ctx context.Context) ([]dockerContainer, error) {
	out, err := core.Run(ctx, "docker", "ps", "--format", "{{.Names}}\t{{.Image}}")
	if err != nil {
		return nil, err
	}
	list := []dockerContainer{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, image, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(image, "mariadb") || strings.HasPrefix(image, "mysql") {
			list = append(list, dockerContainer{Name: name, Image: image})
		}
	}
	return list, nil
}

func (d *Database) instances(w http.ResponseWriter, r *http.Request) {
	list, err := findInstances(r.Context())
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	out := []map[string]string{}
	for _, c := range list {
		out = append(out, map[string]string{"name": c.Name, "image": c.Image})
	}
	core.OK(w, out)
}

// open connects to an instance over its docker network address.
func open(ctx context.Context, inst, db string) (*sql.DB, error) {
	if !validCont.MatchString(inst) {
		return nil, fmt.Errorf("invalid instance")
	}
	out, err := core.Run(ctx, "docker", "inspect", inst, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var info struct {
		Config struct {
			Image string
			Env   []string
		}
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(info.Config.Image, "mariadb") && !strings.HasPrefix(info.Config.Image, "mysql") {
		return nil, fmt.Errorf("%s is not a database container", inst)
	}
	var password string
	for _, e := range info.Config.Env {
		if v, ok := strings.CutPrefix(e, "MYSQL_ROOT_PASSWORD="); ok {
			password = v
		} else if v, ok := strings.CutPrefix(e, "MARIADB_ROOT_PASSWORD="); ok {
			password = v
		}
	}
	var ip string
	for _, n := range info.NetworkSettings.Networks {
		if n.IPAddress != "" {
			ip = n.IPAddress
			break
		}
	}
	if ip == "" {
		return nil, fmt.Errorf("%s has no network address", inst)
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "root", password, "tcp", ip+":3306", db
	cfg.Timeout = 5 * time.Second
	cfg.Collation = "utf8mb4_unicode_ci"
	// Statements like CREATE USER ?@? can't be prepared on the server, so the driver
	// escapes parameters client-side instead.
	cfg.InterpolateParams = true
	conn, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

// quote wraps a validated identifier in backticks.
func quote(ident string) string {
	return "`" + strings.ReplaceAll(ident, "`", "``") + "`"
}

// target opens the connection named by URL parameters, replying itself on failure.
func target(w http.ResponseWriter, r *http.Request, withDB bool) (*sql.DB, bool) {
	db := ""
	if withDB {
		db = chi.URLParam(r, "db")
		if !validIdent.MatchString(db) {
			core.Fail(w, http.StatusBadRequest, "invalid database name")
			return nil, false
		}
	}
	conn, err := open(r.Context(), chi.URLParam(r, "inst"), db)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return nil, false
	}
	return conn, true
}

func (d *Database) databases(w http.ResponseWriter, r *http.Request) {
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	rows, err := conn.QueryContext(r.Context(), `
		SELECT s.schema_name, s.default_collation_name, COALESCE(SUM(t.data_length + t.index_length), 0), COUNT(t.table_name)
		FROM information_schema.schemata s
		LEFT JOIN information_schema.tables t ON t.table_schema = s.schema_name
		GROUP BY s.schema_name, s.default_collation_name ORDER BY s.schema_name`)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, coll string
		var size float64
		var count int
		if rows.Scan(&name, &coll, &size, &count) == nil {
			out = append(out, map[string]any{"name": name, "collation": coll, "size": size, "tables": count, "system": systemDBs[name]})
		}
	}
	core.OK(w, out)
}

// createDatabase optionally creates a matching user, the common case for a new site.
func (d *Database) createDatabase(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, User, Password string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validIdent.MatchString(req.Name) || systemDBs[req.Name] {
		core.Fail(w, http.StatusBadRequest, "invalid database name")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	if _, err := conn.ExecContext(ctx, "CREATE DATABASE "+quote(req.Name)+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.User != "" {
		if !validIdent.MatchString(req.User) || req.Password == "" {
			core.Fail(w, http.StatusBadRequest, "database created, but the user name or password is invalid")
			return
		}
		if _, err := conn.ExecContext(ctx, "CREATE USER ?@'%' IDENTIFIED BY ?", req.User, req.Password); err != nil {
			core.Fail(w, http.StatusBadRequest, "database created, but creating the user failed: %v", err)
			return
		}
		if _, err := conn.ExecContext(ctx, "GRANT ALL PRIVILEGES ON "+quote(req.Name)+".* TO ?@'%'", req.User); err != nil {
			core.Fail(w, http.StatusBadRequest, "database and user created, but the grant failed: %v", err)
			return
		}
	}
	d.app.Audit(r, "db.create", req.Name)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) dropDatabase(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "db")
	if !validIdent.MatchString(name) || systemDBs[name] {
		core.Fail(w, http.StatusForbidden, "this database can't be dropped")
		return
	}
	// The caller repeats the name, so a stray request can't destroy data.
	if r.URL.Query().Get("confirm") != name {
		core.Fail(w, http.StatusBadRequest, "confirmation doesn't match the database name")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), "DROP DATABASE "+quote(name)); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.drop", name)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) tables(w http.ResponseWriter, r *http.Request) {
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	rows, err := conn.QueryContext(r.Context(), `
		SELECT table_name, table_type, COALESCE(engine,''), COALESCE(table_rows,0), COALESCE(data_length+index_length,0)
		FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name`, chi.URLParam(r, "db"))
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var name, kind, engine string
		var count, size float64
		if rows.Scan(&name, &kind, &engine, &count, &size) == nil {
			out = append(out, map[string]any{"name": name, "type": kind, "engine": engine, "rows": count, "size": size})
		}
	}
	core.OK(w, out)
}

type column struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Nullable     bool   `json:"nullable"`
	Key          string `json:"key"`
	DefaultKind  string `json:"defaultKind"`
	DefaultValue string `json:"defaultValue"`
	Extra        string `json:"extra"`
	Comment      string `json:"comment"`
}

func tableColumns(ctx context.Context, conn *sql.DB, db, table string) ([]column, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT column_name, column_type, is_nullable = 'YES', column_key, column_default, extra, column_comment
		FROM information_schema.columns WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`, db, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []column{}
	for rows.Next() {
		var c column
		var def sql.NullString
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Key, &def, &c.Extra, &c.Comment); err != nil {
			return nil, err
		}
		c.DefaultKind, c.DefaultValue = parseDefault(def, c.Nullable)
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("table not found")
	}
	return out, rows.Err()
}

func (d *Database) columns(w http.ResponseWriter, r *http.Request) {
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	cols, err := tableColumns(r.Context(), conn, chi.URLParam(r, "db"), chi.URLParam(r, "table"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	core.OK(w, cols)
}

// scanRows converts a result set to JSON-friendly values. Binary data comes back as a
// placeholder, so the browser never receives megabytes of blob it can't show.
func scanRows(rows *sql.Rows, limit int) ([]string, [][]any, bool, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, nil, false, err
	}
	out := [][]any{}
	for rows.Next() {
		if len(out) >= limit {
			return names, out, true, nil
		}
		raw := make([]sql.RawBytes, len(names))
		ptrs := make([]any, len(names))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}
		row := make([]any, len(names))
		for i, v := range raw {
			switch {
			case v == nil:
				row[i] = nil
			case !utf8.Valid(v) || bytes.IndexByte(v, 0) >= 0:
				row[i] = fmt.Sprintf("[binary %d bytes]", len(v))
			default:
				row[i] = string(v)
			}
		}
		out = append(out, row)
	}
	return names, out, false, rows.Err()
}

func (d *Database) rows(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxRows {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	order := ""
	if by := q.Get("order"); by != "" {
		known := false
		for _, c := range cols {
			known = known || c.Name == by
		}
		if !known {
			core.Fail(w, http.StatusBadRequest, "unknown column")
			return
		}
		order = " ORDER BY " + quote(by)
		if q.Get("dir") == "desc" {
			order += " DESC"
		}
	}
	where, args, err := searchClause(cols, q.Get("search"), q.Get("column"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	res, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s%s%s LIMIT %d OFFSET %d", quote(table), where, order, limit, offset), args...)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	defer res.Close()
	names, data, _, err := scanRows(res, limit)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	var total int64
	_ = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(table)+where, args...).Scan(&total)
	core.OK(w, map[string]any{"columns": names, "rows": data, "total": total, "schema": cols})
}

// primaryKey returns a table's key columns. Editing needs them: without a key, an
// UPDATE could silently hit several identical rows.
func primaryKey(cols []column) []string {
	key := []string{}
	for _, c := range cols {
		if c.Key == "PRI" {
			key = append(key, c.Name)
		}
	}
	return key
}

type rowRequest struct {
	Key    map[string]any `json:"key"`
	Values map[string]any `json:"values"`
}

// whereKey builds a WHERE clause covering the whole primary key.
func whereKey(cols []column, key map[string]any) (string, []any, error) {
	pk := primaryKey(cols)
	if len(pk) == 0 {
		return "", nil, fmt.Errorf("this table has no primary key, so rows can't be edited safely; use the SQL console")
	}
	parts, args := []string{}, []any{}
	for _, name := range pk {
		v, ok := key[name]
		if !ok {
			return "", nil, fmt.Errorf("missing key column %s", name)
		}
		parts = append(parts, quote(name)+" = ?")
		args = append(args, v)
	}
	return strings.Join(parts, " AND "), args, nil
}

func knownColumn(cols []column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (d *Database) rowUpdate(w http.ResponseWriter, r *http.Request) {
	var req rowRequest
	if !core.Read(w, r, &req) {
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	table := chi.URLParam(r, "table")
	cols, err := tableColumns(ctx, conn, chi.URLParam(r, "db"), table)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	where, whereArgs, err := whereKey(cols, req.Key)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	sets, args := []string{}, []any{}
	for name, v := range req.Values {
		if !knownColumn(cols, name) {
			core.Fail(w, http.StatusBadRequest, "unknown column %s", name)
			return
		}
		sets = append(sets, quote(name)+" = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		core.Fail(w, http.StatusBadRequest, "nothing to update")
		return
	}
	res, err := conn.ExecContext(ctx, "UPDATE "+quote(table)+" SET "+strings.Join(sets, ", ")+" WHERE "+where+" LIMIT 1", append(args, whereArgs...)...)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	n, _ := res.RowsAffected()
	d.app.Audit(r, "db.row.update", chi.URLParam(r, "db")+"."+table)
	core.OK(w, map[string]int64{"affected": n})
}

func (d *Database) rowInsert(w http.ResponseWriter, r *http.Request) {
	var req rowRequest
	if !core.Read(w, r, &req) {
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	table := chi.URLParam(r, "table")
	cols, err := tableColumns(ctx, conn, chi.URLParam(r, "db"), table)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	names, marks, args := []string{}, []string{}, []any{}
	for name, v := range req.Values {
		if !knownColumn(cols, name) {
			core.Fail(w, http.StatusBadRequest, "unknown column %s", name)
			return
		}
		names, marks, args = append(names, quote(name)), append(marks, "?"), append(args, v)
	}
	stmt := "INSERT INTO " + quote(table) + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
	if _, err := conn.ExecContext(ctx, stmt, args...); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.row.insert", chi.URLParam(r, "db")+"."+table)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) rowDelete(w http.ResponseWriter, r *http.Request) {
	var req rowRequest
	if !core.Read(w, r, &req) {
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	table := chi.URLParam(r, "table")
	cols, err := tableColumns(ctx, conn, chi.URLParam(r, "db"), table)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	where, args, err := whereKey(cols, req.Key)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	res, err := conn.ExecContext(ctx, "DELETE FROM "+quote(table)+" WHERE "+where+" LIMIT 1", args...)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	n, _ := res.RowsAffected()
	d.app.Audit(r, "db.row.delete", chi.URLParam(r, "db")+"."+table)
	core.OK(w, map[string]int64{"affected": n})
}

// query is the SQL console. It's root access by design, so every statement is audited.
func (d *Database) query(w http.ResponseWriter, r *http.Request) {
	var req struct{ SQL string }
	if !core.Read(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		core.Fail(w, http.StatusBadRequest, "enter a statement")
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(r.Context(), queryWindow)
	defer cancel()
	start := time.Now()
	stmt := req.SQL
	if len(stmt) > 500 {
		stmt = stmt[:500] + "..."
	}
	d.app.Audit(r, "db.query", chi.URLParam(r, "db")+": "+stmt)
	res, err := conn.QueryContext(ctx, req.SQL)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	defer res.Close()
	names, data, truncated, err := scanRows(res, maxRows)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	core.OK(w, map[string]any{"columns": names, "rows": data, "truncated": truncated, "ms": time.Since(start).Milliseconds()})
}

func (d *Database) users(w http.ResponseWriter, r *http.Request) {
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	ctx := r.Context()
	rows, err := conn.QueryContext(ctx, `SELECT user, host FROM mysql.user WHERE user <> '' AND user <> 'mariadb.sys' ORDER BY user, host`)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	type account struct{ user, host string }
	accounts := []account{}
	for rows.Next() {
		var a account
		if rows.Scan(&a.user, &a.host) == nil {
			accounts = append(accounts, a)
		}
	}
	rows.Close()
	out := []map[string]any{}
	for _, a := range accounts {
		dbs := []string{}
		g, err := conn.QueryContext(ctx, `SELECT DISTINCT table_schema FROM information_schema.schema_privileges WHERE grantee = CONCAT("'", ?, "'@'", ?, "'")`, a.user, a.host)
		if err == nil {
			for g.Next() {
				var s string
				if g.Scan(&s) == nil {
					dbs = append(dbs, s)
				}
			}
			g.Close()
		}
		out = append(out, map[string]any{"user": a.user, "host": a.host, "databases": dbs})
	}
	core.OK(w, out)
}

func validAccount(user, host string) error {
	if !validIdent.MatchString(user) {
		return errors.New("invalid user name")
	}
	if !validHost.MatchString(host) {
		return errors.New("invalid host")
	}
	return nil
}

func (d *Database) userCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ User, Host, Password string }
	if !core.Read(w, r, &req) {
		return
	}
	if req.Host == "" {
		req.Host = "%"
	}
	if err := validAccount(req.User, req.Host); err != nil || req.Password == "" {
		core.Fail(w, http.StatusBadRequest, "user, host and password are required")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), "CREATE USER ?@? IDENTIFIED BY ?", req.User, req.Host, req.Password); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.user.create", req.User+"@"+req.Host)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) userPassword(w http.ResponseWriter, r *http.Request) {
	var req struct{ User, Host, Password string }
	if !core.Read(w, r, &req) {
		return
	}
	if err := validAccount(req.User, req.Host); err != nil || req.Password == "" {
		core.Fail(w, http.StatusBadRequest, "user, host and password are required")
		return
	}
	// The panel reads root's password from the container's environment. Changing it
	// here would disconnect the panel, and the app that set it, from the server.
	if req.User == "root" {
		core.Fail(w, http.StatusForbidden, "change root's password in the stack's .env and recreate the container")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), "ALTER USER ?@? IDENTIFIED BY ?", req.User, req.Host, req.Password); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.user.password", req.User+"@"+req.Host)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) userGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User, Host, Database string
		Revoke               bool
	}
	if !core.Read(w, r, &req) {
		return
	}
	if err := validAccount(req.User, req.Host); err != nil || !validIdent.MatchString(req.Database) {
		core.Fail(w, http.StatusBadRequest, "user, host and database are required")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	stmt := "GRANT ALL PRIVILEGES ON " + quote(req.Database) + ".* TO ?@?"
	if req.Revoke {
		stmt = "REVOKE ALL PRIVILEGES ON " + quote(req.Database) + ".* FROM ?@?"
	}
	if _, err := conn.ExecContext(r.Context(), stmt, req.User, req.Host); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.user.grant", fmt.Sprintf("%s@%s %s revoke=%v", req.User, req.Host, req.Database, req.Revoke))
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Database) userDrop(w http.ResponseWriter, r *http.Request) {
	var req struct{ User, Host string }
	if !core.Read(w, r, &req) {
		return
	}
	if err := validAccount(req.User, req.Host); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.User == "root" {
		core.Fail(w, http.StatusForbidden, "root can't be dropped from here")
		return
	}
	conn, ok := target(w, r, false)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), "DROP USER ?@?", req.User, req.Host); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.user.drop", req.User+"@"+req.Host)
	core.OK(w, map[string]bool{"ok": true})
}
