//
//
// Tencent is pleased to support the open source community by making Tencent Cloud AIGate available.
//
// Copyright (C) 2026 Tencent.
// All rights reserved.
//
// If you have downloaded a copy of the Tencent Cloud AIGate source code from Tencent,
// please note that Tencent Cloud AIGate source code is licensed under the Apache 2.0 License,
// A copy of the Apache 2.0 License is included in this file.
//
//

package model

// 白盒单测：GORM 模型与 sql/init.sql 的 schema 一致性。
//
// MySQL 生产不跑 AutoMigrate，全新安装建表完全由 sql/init.sql 负责，它是单一事实源。
// 而 SQLite 本地开发走 AutoMigrate，两条路径各自演进时极易漂移：模型加了字段忘记改
// init.sql，MySQL 上就会在运行期报 Unknown column。这里把漂移挡在单测阶段。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const initSQLPath = "../../sql/init.sql"

// modelTables 返回 SQLite 上由 AutoMigrate 建出的真实表名与列集合，作为「模型一侧」的事实。
func modelTables(t *testing.T, store *Store) map[string]map[string]bool {
	t.Helper()

	db, err := store.db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	tableRows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list sqlite tables: %v", err)
	}
	var names []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := tableRows.Err(); err != nil {
		t.Fatalf("iterate table names: %v", err)
	}
	_ = tableRows.Close()

	out := map[string]map[string]bool{}
	for _, table := range names {
		colRows, err := db.Query("PRAGMA table_info(`" + table + "`)")
		if err != nil {
			t.Fatalf("pragma table_info(%s): %v", table, err)
		}
		out[table] = map[string]bool{}
		for colRows.Next() {
			var (
				cid       int
				name, typ string
				notNull   int
				dflt      any
				pk        int
			)
			if err := colRows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
				t.Fatalf("scan column of %s: %v", table, err)
			}
			out[table][name] = true
		}
		if err := colRows.Err(); err != nil {
			t.Fatalf("iterate columns of %s: %v", table, err)
		}
		_ = colRows.Close()
	}
	return out
}

var (
	createTableRe = regexp.MustCompile("(?s)CREATE TABLE IF NOT EXISTS `(\\w+)` \\((.*?)\\n\\) ENGINE")
	columnLineRe  = regexp.MustCompile("^\\s+`(\\w+)`\\s+\\w")
)

// initSQLTables 解析 sql/init.sql 中每张表的列名。
func initSQLTables(t *testing.T) map[string]map[string]bool {
	t.Helper()

	path, err := filepath.Abs(initSQLPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", initSQLPath, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	out := map[string]map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(string(raw), -1) {
		table, body := m[1], m[2]
		out[table] = map[string]bool{}
		for _, line := range strings.Split(body, "\n") {
			if c := columnLineRe.FindStringSubmatch(line); c != nil {
				out[table][c[1]] = true
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no CREATE TABLE statement parsed from %s", path)
	}
	return out
}

// TestInitSQLMatchesModels 断言 MySQL 建表脚本与 GORM 模型在表、列两个层面完全一致。
// 任何一侧多出或缺少表/列都会失败，并指出具体名称。
func TestInitSQLMatchesModels(t *testing.T) {
	store, err := Open(DBTypeSQLite, "file:"+filepath.Join(t.TempDir(), "schema.db")+"?_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.AutoMigrate(); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}

	fromModels := modelTables(t, store)
	fromSQL := initSQLTables(t)

	for table, wantCols := range fromModels {
		gotCols, ok := fromSQL[table]
		if !ok {
			t.Errorf("sql/init.sql 缺少表 %q（GORM 模型已定义）", table)
			continue
		}
		for col := range wantCols {
			if !gotCols[col] {
				t.Errorf("表 %q 的列 %q 存在于 GORM 模型但 sql/init.sql 未定义", table, col)
			}
		}
		for col := range gotCols {
			if !wantCols[col] {
				t.Errorf("表 %q 的列 %q 存在于 sql/init.sql 但 GORM 模型未定义", table, col)
			}
		}
	}
	for table := range fromSQL {
		if _, ok := fromModels[table]; !ok {
			t.Errorf("sql/init.sql 多出表 %q（GORM 模型未定义，全新安装会建出无人使用的表）", table)
		}
	}
}
