package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

// ColumnDef describes a column to create or change. SQL is always built here from checked
// parts, never taken from the browser, and every change can be previewed first.
type ColumnDef struct {
	Name          string `json:"name"`
	Type          string `json:"type"` // like varchar(255), int unsigned or enum('a','b')
	Nullable      bool   `json:"nullable"`
	DefaultKind   string `json:"defaultKind"` // "", null, value, current_timestamp
	DefaultValue  string `json:"defaultValue"`
	OnUpdate      bool   `json:"onUpdate"`
	AutoIncrement bool   `json:"autoIncrement"`
	Comment       string `json:"comment"`
	Primary       bool   `json:"primary"` // used when creating a table
}

var (
	validNewName = regexp.MustCompile(`^[\p{L}\p{N}_$-]{1,64}$`)
	validEngine  = regexp.MustCompile(`^[A-Za-z]+$`)
	validColl    = regexp.MustCompile(`^[a-z0-9_]+$`)
	plainType    = regexp.MustCompile(`(?i)^([a-z]+)(\s*\(\s*\d+\s*(,\s*\d+\s*)?\))?(\s+unsigned)?(\s+zerofill)?$`)
	listType     = regexp.MustCompile(`(?i)^(enum|set)\s*\(\s*'([^'\\]|'')*'(\s*,\s*'([^'\\]|'')*')*\s*\)$`)
)

var knownTypes = map[string]bool{
	"tinyint": true, "smallint": true, "mediumint": true, "int": true, "integer": true, "bigint": true,
	"decimal": true, "numeric": true, "float": true, "double": true, "bit": true, "bool": true, "boolean": true,
	"char": true, "varchar": true, "binary": true, "varbinary": true,
	"tinytext": true, "text": true, "mediumtext": true, "longtext": true,
	"tinyblob": true, "blob": true, "mediumblob": true, "longblob": true,
	"date": true, "time": true, "datetime": true, "timestamp": true, "year": true, "json": true,
}

// literal quotes a string for use inside a statement. Backslashes are doubled because
// MariaDB treats them as escapes in its default SQL mode.
func literal(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `''`) + "'"
}

func validType(t string) bool {
	if listType.MatchString(t) {
		return true
	}
	m := plainType.FindStringSubmatch(t)
	return m != nil && knownTypes[strings.ToLower(m[1])]
}

// columnSQL builds the definition following the column name in a statement.
func columnSQL(c ColumnDef) (string, error) {
	if !validNewName.MatchString(c.Name) {
		return "", fmt.Errorf("invalid column name %q", c.Name)
	}
	if !validType(c.Type) {
		return "", fmt.Errorf("invalid type %q for column %s", c.Type, c.Name)
	}
	var b strings.Builder
	b.WriteString(quote(c.Name) + " " + c.Type)
	if c.Nullable {
		b.WriteString(" NULL")
	} else {
		b.WriteString(" NOT NULL")
	}
	switch c.DefaultKind {
	case "":
	case "null":
		if !c.Nullable {
			return "", fmt.Errorf("column %s can't default to NULL unless it allows NULL", c.Name)
		}
		b.WriteString(" DEFAULT NULL")
	case "value":
		b.WriteString(" DEFAULT " + literal(c.DefaultValue))
	case "current_timestamp":
		b.WriteString(" DEFAULT CURRENT_TIMESTAMP")
	default:
		return "", fmt.Errorf("invalid default for column %s", c.Name)
	}
	if c.OnUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP")
	}
	if c.AutoIncrement {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT " + literal(c.Comment))
	}
	return b.String(), nil
}

// CreateRequest is a new table.
type CreateRequest struct {
	Name      string      `json:"name"`
	Engine    string      `json:"engine"`
	Collation string      `json:"collation"`
	Comment   string      `json:"comment"`
	Columns   []ColumnDef `json:"columns"`
	Dry       bool        `json:"dry"`
}

func createSQL(req CreateRequest) (string, error) {
	if !validNewName.MatchString(req.Name) {
		return "", fmt.Errorf("invalid table name %q", req.Name)
	}
	if len(req.Columns) == 0 {
		return "", fmt.Errorf("add at least one column")
	}
	lines, keys := []string{}, []string{}
	for _, c := range req.Columns {
		def, err := columnSQL(c)
		if err != nil {
			return "", err
		}
		lines = append(lines, "  "+def)
		if c.Primary {
			keys = append(keys, quote(c.Name))
		}
	}
	if len(keys) > 0 {
		lines = append(lines, "  PRIMARY KEY ("+strings.Join(keys, ", ")+")")
	}
	engine, coll := req.Engine, req.Collation
	if engine == "" {
		engine = "InnoDB"
	}
	if coll == "" {
		coll = "utf8mb4_unicode_ci"
	}
	if !validEngine.MatchString(engine) || !validColl.MatchString(coll) {
		return "", fmt.Errorf("invalid engine or collation")
	}
	stmt := "CREATE TABLE " + quote(req.Name) + " (\n" + strings.Join(lines, ",\n") + "\n) ENGINE=" + engine + " COLLATE=" + coll
	if req.Comment != "" {
		stmt += " COMMENT=" + literal(req.Comment)
	}
	return stmt, nil
}

// AlterRequest is one change to an existing table.
type AlterRequest struct {
	Op      string    `json:"op"` // addColumn, modifyColumn, dropColumn, addIndex, dropIndex, rename
	Name    string    `json:"name"`
	Column  ColumnDef `json:"column"`
	After   string    `json:"after"` // "" keeps the position, "^" means first, else a column name
	Kind    string    `json:"kind"`  // index, unique, primary, fulltext
	Columns []string  `json:"columns"`
	To      string    `json:"to"`
	Dry     bool      `json:"dry"`
}

func alterSQL(table string, cols []column, req AlterRequest) (string, error) {
	prefix := "ALTER TABLE " + quote(table) + " "
	known := func(name string) bool { return knownColumn(cols, name) }
	position := func() (string, error) {
		switch {
		case req.After == "":
			return "", nil
		case req.After == "^":
			return " FIRST", nil
		case known(req.After):
			return " AFTER " + quote(req.After), nil
		}
		return "", fmt.Errorf("unknown column %q", req.After)
	}
	switch req.Op {
	case "addColumn":
		def, err := columnSQL(req.Column)
		if err != nil {
			return "", err
		}
		pos, err := position()
		if err != nil {
			return "", err
		}
		return prefix + "ADD COLUMN " + def + pos, nil
	case "modifyColumn":
		if !known(req.Name) {
			return "", fmt.Errorf("unknown column %q", req.Name)
		}
		def, err := columnSQL(req.Column)
		if err != nil {
			return "", err
		}
		pos, err := position()
		if err != nil {
			return "", err
		}
		return prefix + "CHANGE COLUMN " + quote(req.Name) + " " + def + pos, nil
	case "dropColumn":
		if !known(req.Name) {
			return "", fmt.Errorf("unknown column %q", req.Name)
		}
		if len(cols) == 1 {
			return "", fmt.Errorf("a table needs at least one column; drop the table instead")
		}
		return prefix + "DROP COLUMN " + quote(req.Name), nil
	case "addIndex":
		if len(req.Columns) == 0 {
			return "", fmt.Errorf("choose at least one column")
		}
		parts := []string{}
		for _, c := range req.Columns {
			if !known(c) {
				return "", fmt.Errorf("unknown column %q", c)
			}
			parts = append(parts, quote(c))
		}
		list := "(" + strings.Join(parts, ", ") + ")"
		if req.Kind == "primary" {
			return prefix + "ADD PRIMARY KEY " + list, nil
		}
		keyword, ok := map[string]string{"index": "INDEX", "unique": "UNIQUE INDEX", "fulltext": "FULLTEXT INDEX"}[req.Kind]
		if !ok {
			return "", fmt.Errorf("invalid index kind")
		}
		name := req.Name
		if name == "" {
			name = strings.Join(req.Columns, "_")
			if len(name) > 64 {
				name = name[:64]
			}
		}
		if !validNewName.MatchString(name) {
			return "", fmt.Errorf("invalid index name %q", name)
		}
		return prefix + "ADD " + keyword + " " + quote(name) + " " + list, nil
	case "dropIndex":
		if req.Name == "PRIMARY" {
			return prefix + "DROP PRIMARY KEY", nil
		}
		if !validNewName.MatchString(req.Name) {
			return "", fmt.Errorf("invalid index name")
		}
		return prefix + "DROP INDEX " + quote(req.Name), nil
	case "rename":
		if !validNewName.MatchString(req.To) {
			return "", fmt.Errorf("invalid table name %q", req.To)
		}
		return "RENAME TABLE " + quote(table) + " TO " + quote(req.To), nil
	}
	return "", fmt.Errorf("unknown operation %q", req.Op)
}

// parseDefault turns information_schema's column_default into something a form can edit.
// MariaDB reports strings quoted and "no default" as SQL NULL, with the text NULL meaning
// DEFAULT NULL.
func parseDefault(def sql.NullString, nullable bool) (kind, value string) {
	if !def.Valid {
		if nullable {
			return "null", ""
		}
		return "", ""
	}
	v := def.String
	switch {
	case strings.EqualFold(v, "NULL"):
		return "null", ""
	case strings.HasPrefix(strings.ToLower(v), "current_timestamp"):
		return "current_timestamp", ""
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return "value", strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return "value", v
}

// writableDB replies itself when the URL names a system database.
func writableDB(w http.ResponseWriter, r *http.Request) bool {
	if systemDBs[chi.URLParam(r, "db")] {
		core.Fail(w, http.StatusForbidden, "system databases can't be changed here; use the SQL console if you must")
		return false
	}
	return true
}

func (d *Database) tableCreate(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !core.Read(w, r, &req) || !writableDB(w, r) {
		return
	}
	stmt, err := createSQL(req)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Dry {
		core.OK(w, map[string]string{"sql": stmt})
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), stmt); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.table.create", chi.URLParam(r, "db")+"."+req.Name)
	core.OK(w, map[string]string{"sql": stmt})
}

func (d *Database) tableAlter(w http.ResponseWriter, r *http.Request) {
	var req AlterRequest
	if !core.Read(w, r, &req) || !writableDB(w, r) {
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	table := chi.URLParam(r, "table")
	cols, err := tableColumns(r.Context(), conn, chi.URLParam(r, "db"), table)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	stmt, err := alterSQL(table, cols, req)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if req.Dry {
		core.OK(w, map[string]string{"sql": stmt})
		return
	}
	if _, err := conn.ExecContext(r.Context(), stmt); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, "db.table.alter", chi.URLParam(r, "db")+": "+stmt)
	core.OK(w, map[string]string{"sql": stmt})
}

// tableDrop and tableTruncate need the name repeated, so a stray request can't destroy data.
func (d *Database) tableDrop(w http.ResponseWriter, r *http.Request) {
	d.tableDestroy(w, r, "DROP TABLE ", "db.table.drop")
}

func (d *Database) tableTruncate(w http.ResponseWriter, r *http.Request) {
	d.tableDestroy(w, r, "TRUNCATE TABLE ", "db.table.truncate")
}

func (d *Database) tableDestroy(w http.ResponseWriter, r *http.Request, verb, action string) {
	if !writableDB(w, r) {
		return
	}
	table := chi.URLParam(r, "table")
	if r.URL.Query().Get("confirm") != table {
		core.Fail(w, http.StatusBadRequest, "confirmation doesn't match the table name")
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	if _, err := conn.ExecContext(r.Context(), verb+quote(table)); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	d.app.Audit(r, action, chi.URLParam(r, "db")+"."+table)
	core.OK(w, map[string]bool{"ok": true})
}

// structure returns everything the Structure tab shows.
func (d *Database) structure(w http.ResponseWriter, r *http.Request) {
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
	type index struct {
		Name    string   `json:"name"`
		Unique  bool     `json:"unique"`
		Kind    string   `json:"kind"`
		Columns []string `json:"columns"`
	}
	rows, err := conn.QueryContext(ctx, `SELECT index_name, non_unique = 0, index_type, column_name FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ? ORDER BY index_name, seq_in_index`, db, table)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	defer rows.Close()
	indexes := []*index{}
	byName := map[string]*index{}
	for rows.Next() {
		var name, kind, col string
		var unique bool
		if rows.Scan(&name, &unique, &kind, &col) != nil {
			continue
		}
		ix := byName[name]
		if ix == nil {
			ix = &index{Name: name, Unique: unique, Kind: kind}
			byName[name] = ix
			indexes = append(indexes, ix)
		}
		ix.Columns = append(ix.Columns, col)
	}
	var kind, engine, coll, comment, create string
	var count, size float64
	var ignored string
	_ = conn.QueryRowContext(ctx, `SELECT table_type, COALESCE(engine,''), COALESCE(table_collation,''), COALESCE(table_comment,''), COALESCE(table_rows,0), COALESCE(data_length+index_length,0)
		FROM information_schema.tables WHERE table_schema = ? AND table_name = ?`, db, table).Scan(&kind, &engine, &coll, &comment, &count, &size)
	if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+quote(table)).Scan(&ignored, &create); err != nil {
		create = "" // views return different columns; the page simply leaves this out
	}
	core.OK(w, map[string]any{
		"columns": cols, "indexes": indexes, "type": kind, "engine": engine, "collation": coll,
		"comment": comment, "rows": count, "size": size, "create": create,
	})
}

// textLike reports whether searching a column makes sense. Blobs can't usefully match
// text, and scanning them is what would make a search slow.
func textLike(columnType string) bool {
	t := strings.ToLower(columnType)
	for _, skip := range []string{"blob", "binary", "bit", "geometry", "point", "polygon", "linestring"} {
		if strings.Contains(t, skip) {
			return false
		}
	}
	return true
}

func likePattern(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(term) + "%"
}

// searchClause builds a WHERE clause matching term in one column or every searchable one.
func searchClause(cols []column, term, only string) (string, []any, error) {
	if term == "" {
		return "", nil, nil
	}
	parts, args := []string{}, []any{}
	pattern := likePattern(term)
	for _, c := range cols {
		if only != "" && c.Name != only {
			continue
		}
		if only == "" && !textLike(c.Type) {
			continue
		}
		parts = append(parts, "CAST("+quote(c.Name)+" AS CHAR) LIKE ?")
		args = append(args, pattern)
	}
	if len(parts) == 0 {
		if only != "" {
			return "", nil, fmt.Errorf("unknown column %q", only)
		}
		return "", nil, fmt.Errorf("this table has no searchable columns")
	}
	return " WHERE (" + strings.Join(parts, " OR ") + ")", args, nil
}

// search counts matches table by table, within the query window. On a big database it may
// not get through everything, and says so rather than reporting a misleading result.
func (d *Database) search(w http.ResponseWriter, r *http.Request) {
	var req struct{ Term string }
	if !core.Read(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Term) == "" {
		core.Fail(w, http.StatusBadRequest, "enter something to search for")
		return
	}
	conn, ok := target(w, r, true)
	if !ok {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(r.Context(), queryWindow)
	defer cancel()
	db := chi.URLParam(r, "db")
	rows, err := conn.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE' ORDER BY table_name`, db)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	names := []string{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	matches := []map[string]any{}
	searched, incomplete := 0, false
	for _, table := range names {
		if ctx.Err() != nil {
			incomplete = true
			break
		}
		cols, err := tableColumns(ctx, conn, db, table)
		if err != nil {
			continue
		}
		where, args, err := searchClause(cols, req.Term, "")
		if err != nil {
			searched++
			continue
		}
		var count int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quote(table)+where, args...).Scan(&count); err != nil {
			if ctx.Err() != nil {
				incomplete = true
				break
			}
			continue
		}
		searched++
		if count > 0 {
			matches = append(matches, map[string]any{"table": table, "count": count})
		}
	}
	core.OK(w, map[string]any{"matches": matches, "searched": searched, "tables": len(names), "incomplete": incomplete})
}
