package main

import (
	"encoding/json"
	"fmt"
	"github.com/Telara-Labs/TAP-Runtime/discover/util"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Telara-Labs/TAP-Runtime/discover/redact"
)

// Generic captures for agents whose sessions are SQLite tables, JSON files
// or text (TENG-3118 to TENG-3121). Every string is scrubbed (credentials
// redacted, the home directory replaced) and a database becomes the SQL
// that rebuilds it, never a binary.
//
//	discover-fixture sqlite <db> <out.sql> <table>[:<where>]...
//	discover-fixture json <in.json> <out.json>
//	discover-fixture text <in> <out>

func sqliteCapture(db, out string, specs []string) error {
	home, _ := os.UserHomeDir()
	var sql strings.Builder
	for _, spec := range specs {
		table, where, _ := strings.Cut(spec, ":")
		schema, err := exec.Command("sqlite3", "-readonly", util.SQLiteURI(db), ".schema "+table).Output()
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		// Only the CREATE TABLE: triggers and indexes are not needed to read.
		for _, stmt := range strings.SplitAfter(string(schema), ";") {
			if strings.Contains(stmt, "CREATE TABLE") {
				sql.WriteString(strings.TrimSpace(stmt) + "\n")
			}
		}
		q := "SELECT * FROM " + table
		if where != "" {
			q += " WHERE " + where
		}
		raw, err := exec.Command("sqlite3", "-readonly", "-json", util.SQLiteURI(db), q).Output()
		if err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		var rows []map[string]any
		if len(strings.TrimSpace(string(raw))) > 0 {
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			if err := dec.Decode(&rows); err != nil {
				return err
			}
		}
		cols, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		for _, r := range rows {
			var vals []string
			for _, c := range cols {
				vals = append(vals, sqlLiteral(r[c], home))
			}
			fmt.Fprintf(&sql, "INSERT INTO %s (%s) VALUES (%s);\n", table, strings.Join(quoteIdents(cols), ", "), strings.Join(vals, ", "))
		}
		fmt.Fprintf(os.Stderr, "%s: %d rows\n", table, len(rows))
	}
	return writeFile(out, sql.String())
}

func tableColumns(db, table string) ([]string, error) {
	raw, err := exec.Command("sqlite3", "-readonly", "-json", util.SQLiteURI(db), "SELECT name FROM pragma_table_info('"+table+"')").Output()
	if err != nil {
		return nil, err
	}
	var rows []struct{ Name string }
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	var cols []string
	for _, r := range rows {
		cols = append(cols, r.Name)
	}
	return cols, nil
}

func quoteIdents(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = `"` + c + `"`
	}
	return out
}

// sqlLiteral writes one value; a text value that holds JSON is scrubbed as
// JSON, so the structure survives redaction.
func sqlLiteral(v any, home string) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "1"
		}
		return "0"
	case string:
		s := x
		var doc any
		if t := strings.TrimSpace(s); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Unmarshal([]byte(s), &doc) == nil {
			s = string(marshal(scrub(doc, home)))
		} else {
			s = scrub(s, home).(string)
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
}

func jsonCapture(in, out string) error {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	return writeFile(out, string(marshal(scrub(doc, home)))+"\n")
}

func textCapture(in, out string) error {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		lines = append(lines, scrubLine(l, home))
	}
	return writeFile(out, strings.Join(lines, "\n"))
}

// scrubLine redacts a text line and replaces the home directory, keeping
// the line whole.
func scrubLine(l, home string) string {
	s := redact.Redact(l)
	if home != "" {
		s = strings.ReplaceAll(s, home, "/home/user")
		s = strings.ReplaceAll(s, filepath.Base(home), "user")
	}
	return s
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}
