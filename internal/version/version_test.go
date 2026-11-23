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

package version_test

import (
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"aigate/internal/version"
)

// TestGetNeverEmpty 覆盖字段兜底：无论是否注入、是否在 VCS 中构建，
// Get 返回的所有字段都不会是空串，避免 HTTP 响应出现 "" 让人误判。
func TestGetNeverEmpty(t *testing.T) {
	t.Chdir(t.TempDir()) // 脱离仓库目录，模拟无 VCS 元数据的构建产物
	info := version.Get()
	if info.Version == "" {
		t.Error("Version 不应为空")
	}
	if info.Commit == "" || info.BuildTime == "" {
		t.Errorf("Commit/BuildTime 不应为空，实际 %q/%q", info.Commit, info.BuildTime)
	}
	if info.GoVersion == "" {
		t.Error("GoVersion 不应为空")
	}
	if info.StartedAt == "" {
		t.Error("StartedAt 不应为空")
	}
}

// TestStartRecordsOnce 覆盖启动时间只记录首次：重复调用不刷新。
func TestStartRecordsOnce(t *testing.T) {
	version.Start()
	first := version.Get().StartedAt
	time.Sleep(10 * time.Millisecond)
	version.Start()
	if second := version.Get().StartedAt; second != first {
		t.Fatalf("StartedAt 被刷新：%q → %q", first, second)
	}
	if _, err := time.Parse(time.RFC3339, first); err != nil {
		t.Fatalf("StartedAt %q 不是 RFC3339：%v", first, err)
	}
}

// TestInjectedCommitWins 覆盖注入值优先于 VCS 元数据：注入 commit 后 Get 必须原样返回。
func TestInjectedCommitWins(t *testing.T) {
	orig := version.Commit
	t.Cleanup(func() { version.Commit = orig })
	version.Commit = "deadbeef"
	if got := version.Get().Commit; got != "deadbeef" {
		t.Fatalf("Commit = %q，期望注入值 deadbeef", got)
	}
}

// TestStringRendersAllFields 覆盖 `-version` 输出包含版本、commit、dirty 与构建信息。
func TestStringRendersAllFields(t *testing.T) {
	out := version.Get().String()
	for _, want := range []string{version.Version, "commit:", "dirty:", "build time:", "go version:"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
	if bi, ok := debug.ReadBuildInfo(); ok && !strings.Contains(out, bi.GoVersion) {
		t.Errorf("输出未包含 Go 版本 %q：\n%s", bi.GoVersion, out)
	}
}
