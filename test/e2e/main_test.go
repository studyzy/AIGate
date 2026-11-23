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

import (
	"fmt"
	"os"
	"testing"
)

// TestMain 负责整套用例跑完后的全局清理。
//
// 各用例在 t.Cleanup 里已停掉自己启动的网关进程；这里兜底删除 SQLite 测试库（含 -wal/-shm），
// 确保下一次运行从空环境开始。MySQL 的库重置与容器生命周期由 test/e2e/run.sh 负责。
func TestMain(m *testing.M) {
	code := m.Run()

	if dbType() == dbTypeSQLite {
		if dsn := os.Getenv(envDSNKey); dsn != "" {
			if err := removeSQLiteFiles(dsn); err != nil {
				fmt.Fprintf(os.Stderr, "[e2e] failed to remove the SQLite test database %s: %v\n", dsn, err)
				if code == 0 {
					code = 1
				}
			}
		}
	}

	os.Exit(code)
}

// removeSQLiteFiles 删除 SQLite 库文件及 WAL 附属文件，忽略不存在的情况。
func removeSQLiteFiles(dsn string) error {
	for _, p := range []string{dsn, dsn + "-wal", dsn + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
