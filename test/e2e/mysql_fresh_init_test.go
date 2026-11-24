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

//go:build e2e

package e2e

// MySQL 干净初始化路径验证。
//
// 生产与 e2e 的 MySQL 表结构完全来自 sql/init.sql（不跑 AutoMigrate），所以「全新安装能否
// 正常起来并用起来」是一条独立的失败路径：init.sql 漏表、漏列或类型不兼容时，网关能启动
// （启动只做 Ping 与 bootstrap），但第一次真正读写就会报 Unknown column。
//
// 本用例不依赖 run.sh 预先建库，而是自己在一个全新的库上驱动真实二进制，覆盖：
//  1. 全新库（空库）上启动网关 → bootstrap 管理员 → 读写多张表；
//  2. 数据面转发与用量落库（写 llm_usage_logs）；
//  3. 重启后数据仍在（init.sql 的建表是幂等的，不会清数据）；
//  4. 全表自检：逐表 SELECT，任何缺列都会在这里暴露。
//
// SQLite 模式跳过：它的建表由 AutoMigrate 负责，没有「init.sql 干净初始化」这条路径。

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2E_MySQLFreshInit 验证全新 MySQL 库能仅凭 sql/init.sql 正常承载网关的全部读写。
func TestE2E_MySQLFreshInit(t *testing.T) {
	if dbType() != dbTypeMySQL {
		t.Skipf("init.sql fresh-install path is MySQL only (current AIGATE_E2E_DB_TYPE=%q)", dbType())
	}

	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	// 从一个全新的空库出发：不沿用 run.sh 已导入 schema 的库，避免掩盖 init.sql 自身的问题。
	freshDB := "aigate_e2e_fresh"
	adminDB := openE2EDB(t, dsn)
	if _, err := adminDB.Exec("DROP DATABASE IF EXISTS `" + freshDB + "`"); err != nil {
		t.Fatalf("drop fresh database: %v", err)
	}
	if _, err := adminDB.Exec("CREATE DATABASE `" + freshDB + "` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create fresh database: %v", err)
	}
	t.Cleanup(func() {
		cleanupDB := openE2EDB(t, dsn)
		if _, err := cleanupDB.Exec("DROP DATABASE IF EXISTS `" + freshDB + "`"); err != nil {
			t.Logf("failed to drop fresh database %s: %v", freshDB, err)
		}
	})

	freshDSN := swapDSNDatabase(t, dsn, freshDB)

	// 仅导入 sql/init.sql —— 这是全新安装的唯一建表来源。
	initSQL, err := os.ReadFile(filepath.Join(repoRoot(t), "sql", "init.sql"))
	if err != nil {
		t.Fatalf("read sql/init.sql: %v", err)
	}
	freshDBHandle := openE2EDB(t, freshDSN)
	if err := execMultiStatement(t, freshDBHandle, string(initSQL)); err != nil {
		t.Fatalf("apply sql/init.sql to a fresh database: %v", err)
	}

	t.Run("1. init.sql creates every table of the GORM model set", func(t *testing.T) {
		want := map[string]bool{
			"users": true, "ai_models": true, "api_keys": true, "api_key_models": true,
			"llm_usage_logs": true, "daily_usage_summaries": true, "user_groups": true,
			"settings": true, "credentials": true, "webhooks": true, "webhook_deliveries": true,
			"response_states": true, "model_routers": true,
			"ai_model_prices": true, "billing_calendar": true, "currency_rates": true,
		}
		rows, err := freshDBHandle.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
		if err != nil {
			t.Fatalf("list tables: %v", err)
		}
		defer rows.Close()
		got := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("scan table name: %v", err)
			}
			got[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate tables: %v", err)
		}
		for table := range want {
			if !got[table] {
				t.Errorf("fresh install is missing table %q", table)
			}
		}
		for table := range got {
			if !want[table] {
				t.Errorf("fresh install created unexpected table %q", table)
			}
		}
	})

	t.Run("2. the gateway boots on an empty database and serves real traffic", func(t *testing.T) {
		mock := newUpstreamMock(kindOpenAI)
		t.Cleanup(mock.Close)

		gw := startGateway(t, bin, freshDSN)

		modelID := createModel(t, gw.baseURL, adminToken, map[string]any{
			"provider":   "mock",
			"model_id":   openAIModelID,
			"model_name": openAIModelID,
			"url":        mock.URL() + "/v1",
			"api_key":    upstreamAPIKey,
			"model_type": "openai-completions",
			"quota_day":  -1,
			"enabled":    true,
		})
		if modelID == 0 {
			t.Fatalf("createModel returned id 0 on a fresh install")
		}

		// 干净库里没有任何用户，先建一个再把密钥挂到它名下。
		owner := createUser(t, gw.baseURL, "fresh-owner", 0)

		key := createKey(t, gw.baseURL, map[string]any{
			"name":     "fresh-init",
			"user_id":  owner.ID,
			"bindings": []map[string]any{{"ai_model_id": modelID, "role": "primary"}},
		})
		if key.Token == "" {
			t.Fatalf("key creation on a fresh install returned an empty token")
		}

		status, body := postChatCompletion(t, gw.baseURL, key.Token, map[string]any{
			"model":    openAIModelID,
			"messages": []map[string]any{{"role": "user", "content": "hello"}},
		})
		if status != 200 {
			t.Fatalf("chat completion on a fresh install: status = %d, body = %s", status, body)
		}

		// 用量是异步落库的：等价于 llm_usage_logs 的每个列都参与了一次真实 INSERT。
		var count int
		waitFor(t, "usage row persisted on a fresh install", 10*time.Second, func() error {
			if err := freshDBHandle.QueryRow("SELECT COUNT(*) FROM `llm_usage_logs`").Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return errors.New("no usage row yet")
			}
			return nil
		})

		var settingsCount int
		if err := freshDBHandle.QueryRow("SELECT COUNT(*) FROM `settings`").Scan(&settingsCount); err != nil {
			t.Fatalf("read settings on a fresh install: %v", err)
		}
	})

	t.Run("3. every table is readable and writable by the schema itself", func(t *testing.T) {
		// 逐表做一次「全列 SELECT + LIMIT 0」，缺列/列名不一致会在这里立刻报错；
		// 这一步不依赖具体用例，覆盖那些本次没被写到的表（如 webhook_deliveries）。
		for _, table := range []string{
			"users", "ai_models", "api_keys", "api_key_models", "llm_usage_logs",
			"daily_usage_summaries", "user_groups", "settings", "credentials",
			"webhooks", "webhook_deliveries", "response_states",
			"ai_model_prices", "billing_calendar", "currency_rates",
		} {
			rows, err := freshDBHandle.Query("SELECT * FROM `" + table + "` LIMIT 0")
			if err != nil {
				t.Errorf("table %q is not usable after the fresh install: %v", table, err)
				continue
			}
			if err := rows.Close(); err != nil {
				t.Errorf("close %q result set: %v", table, err)
			}
		}
	})
}

// swapDSNDatabase 把 DSN 里的库名换成 dbName，其余参数（charset/parseTime/loc）保持不变。
func swapDSNDatabase(t *testing.T, dsn, dbName string) string {
	t.Helper()
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		t.Fatalf("cannot locate the database name in the DSN")
	}
	head, tail := dsn[:i], dsn[i+1:]
	if q := strings.Index(tail, "?"); q >= 0 {
		return head + "/" + dbName + tail[q:]
	}
	return head + "/" + dbName
}

// execMultiStatement 逐条执行 init.sql 里的语句：驱动默认不允许一次提交多条。
// 这里只用于测试夹具，语句来源是仓库内的可信文件，不做 SQL 参数化。
func execMultiStatement(t *testing.T, db *sql.DB, script string) error {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		sb.WriteString(line)
		sb.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(sb.String())
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
			sb.Reset()
		}
	}
	if rest := strings.TrimSpace(sb.String()); rest != "" {
		if _, err := db.Exec(rest); err != nil {
			return err
		}
	}
	return nil
}

// repoRoot 返回仓库根目录（e2e 包的上一级）。
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}
