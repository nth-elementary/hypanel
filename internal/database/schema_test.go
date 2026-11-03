package database

import (
	"database/sql"
	"strings"
	"testing"
)

func TestColumnSQL(t *testing.T) {
	got, err := columnSQL(ColumnDef{Name: "title", Type: "varchar(255)", DefaultKind: "value", DefaultValue: `it's a \ test`, Comment: "the title"})
	if err != nil {
		t.Fatal(err)
	}
	want := "`title` varchar(255) NOT NULL DEFAULT 'it''s a \\\\ test' COMMENT 'the title'"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	got, err = columnSQL(ColumnDef{Name: "at", Type: "timestamp", Nullable: true, DefaultKind: "current_timestamp", OnUpdate: true})
	if err != nil || got != "`at` timestamp NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP" {
		t.Errorf("%q %v", got, err)
	}
}

// Column definitions become statements, so nothing may smuggle in extra SQL.
func TestColumnSQLRejectsInjection(t *testing.T) {
	for name, c := range map[string]ColumnDef{
		"type":     {Name: "a", Type: "int; DROP TABLE x"},
		"type2":    {Name: "a", Type: "int) ; --"},
		"unknown":  {Name: "a", Type: "nonsense"},
		"name":     {Name: "a`b", Type: "int"},
		"enum":     {Name: "a", Type: "enum('x'); DROP TABLE y; --')"},
		"default":  {Name: "a", Type: "int", DefaultKind: "raw"},
		"nullable": {Name: "a", Type: "int", DefaultKind: "null"},
	} {
		if _, err := columnSQL(c); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	for _, ok := range []string{"int", "INT(11) unsigned", "decimal(10,2)", "enum('a','it''s')", "set('x')", "longtext", "datetime"} {
		if !validType(ok) {
			t.Errorf("%s should be valid", ok)
		}
	}
}

func TestLiteralCannotBreakOut(t *testing.T) {
	if got := literal(`\'; DROP TABLE x; --`); got != `'\\''; DROP TABLE x; --'` {
		t.Errorf("%s", got)
	}
}

func TestCreateSQL(t *testing.T) {
	got, err := createSQL(CreateRequest{Name: "posts", Columns: []ColumnDef{
		{Name: "id", Type: "int", AutoIncrement: true, Primary: true},
		{Name: "body", Type: "text", Nullable: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE TABLE `posts`", "`id` int NOT NULL AUTO_INCREMENT", "`body` text NULL", "PRIMARY KEY (`id`)", "ENGINE=InnoDB", "utf8mb4_unicode_ci"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if _, err := createSQL(CreateRequest{Name: "x"}); err == nil {
		t.Error("no columns should be refused")
	}
	if _, err := createSQL(CreateRequest{Name: "x", Engine: "InnoDB; DROP", Columns: []ColumnDef{{Name: "a", Type: "int"}}}); err == nil {
		t.Error("bad engine should be refused")
	}
}

func TestAlterSQL(t *testing.T) {
	cols := []column{{Name: "id"}, {Name: "title"}}
	for _, c := range []struct {
		req  AlterRequest
		want string
	}{
		{AlterRequest{Op: "addColumn", Column: ColumnDef{Name: "x", Type: "int"}, After: "id"}, "ALTER TABLE `t` ADD COLUMN `x` int NOT NULL AFTER `id`"},
		{AlterRequest{Op: "addColumn", Column: ColumnDef{Name: "x", Type: "int"}, After: "^"}, "ALTER TABLE `t` ADD COLUMN `x` int NOT NULL FIRST"},
		{AlterRequest{Op: "modifyColumn", Name: "title", Column: ColumnDef{Name: "heading", Type: "text", Nullable: true}}, "ALTER TABLE `t` CHANGE COLUMN `title` `heading` text NULL"},
		{AlterRequest{Op: "dropColumn", Name: "title"}, "ALTER TABLE `t` DROP COLUMN `title`"},
		{AlterRequest{Op: "addIndex", Kind: "unique", Columns: []string{"title"}}, "ALTER TABLE `t` ADD UNIQUE INDEX `title` (`title`)"},
		{AlterRequest{Op: "addIndex", Kind: "primary", Columns: []string{"id"}}, "ALTER TABLE `t` ADD PRIMARY KEY (`id`)"},
		{AlterRequest{Op: "dropIndex", Name: "PRIMARY"}, "ALTER TABLE `t` DROP PRIMARY KEY"},
		{AlterRequest{Op: "rename", To: "t2"}, "RENAME TABLE `t` TO `t2`"},
	} {
		got, err := alterSQL("t", cols, c.req)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", c.req.Op, got, err, c.want)
		}
	}
	for name, req := range map[string]AlterRequest{
		"unknown drop":   {Op: "dropColumn", Name: "nope"},
		"unknown modify": {Op: "modifyColumn", Name: "nope", Column: ColumnDef{Name: "a", Type: "int"}},
		"unknown after":  {Op: "addColumn", Column: ColumnDef{Name: "x", Type: "int"}, After: "nope"},
		"unknown index":  {Op: "addIndex", Kind: "index", Columns: []string{"nope"}},
		"bad kind":       {Op: "addIndex", Kind: "evil", Columns: []string{"id"}},
		"bad rename":     {Op: "rename", To: "a`b"},
		"bad op":         {Op: "raw"},
	} {
		if _, err := alterSQL("t", cols, req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if _, err := alterSQL("t", cols[:1], AlterRequest{Op: "dropColumn", Name: "id"}); err == nil {
		t.Error("dropping the last column should be refused")
	}
}

func TestParseDefault(t *testing.T) {
	for _, c := range []struct {
		in         sql.NullString
		nullable   bool
		kind, want string
	}{
		{sql.NullString{}, false, "", ""},
		{sql.NullString{}, true, "null", ""},
		{sql.NullString{String: "NULL", Valid: true}, true, "null", ""},
		{sql.NullString{String: "current_timestamp()", Valid: true}, false, "current_timestamp", ""},
		{sql.NullString{String: "'it''s'", Valid: true}, false, "value", "it's"},
		{sql.NullString{String: "0", Valid: true}, false, "value", "0"},
	} {
		kind, value := parseDefault(c.in, c.nullable)
		if kind != c.kind || value != c.want {
			t.Errorf("%+v: got %q %q", c.in, kind, value)
		}
	}
}

func TestSearchClause(t *testing.T) {
	cols := []column{{Name: "id", Type: "int"}, {Name: "title", Type: "varchar(9)"}, {Name: "data", Type: "longblob"}}
	where, args, err := searchClause(cols, "50%_x", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(where, "data") || !strings.Contains(where, "`title`") || !strings.Contains(where, "`id`") {
		t.Errorf("where: %s", where)
	}
	if len(args) != 2 || args[0] != `%50\%\_x%` {
		t.Errorf("args: %v", args)
	}
	where, args, err = searchClause(cols, "x", "title")
	if err != nil || len(args) != 1 || strings.Contains(where, "`id`") {
		t.Errorf("one column: %s %v %v", where, args, err)
	}
	if _, _, err := searchClause(cols, "x", "nope"); err == nil {
		t.Error("unknown column should be refused")
	}
	if where, _, _ := searchClause(cols, "", ""); where != "" {
		t.Error("empty term means no filter")
	}
}
