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

package requestlog_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aigate/internal/requestlog"
)

// 同时给 inline 与 file 时，file 优先；返回值必须是文件内容本身。
func TestLoadRawConfig_FileWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rl.json")
	want := []byte(`{"path":"request-logs.db","retention_days":7}`)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("写配置文件失败：%v", err)
	}
	// inline 故意给一个同样合法但不同的对象，用来验证优先级。
	got, err := requestlog.LoadRawConfig(`{"inline":true}`, path)
	if err != nil {
		t.Fatalf("LoadRawConfig 失败：%v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("应优先返回文件内容，实得 %q，期望 %q", got, want)
	}
	if !json.Valid(got) {
		t.Errorf("返回值应为合法 JSON：%q", got)
	}
}

// inline 与 file 都为空时返回空对象 "{}"，且是合法 JSON 对象。
func TestLoadRawConfig_EmptyDefaults(t *testing.T) {
	got, err := requestlog.LoadRawConfig("", "")
	if err != nil {
		t.Fatalf("LoadRawConfig(\"\", \"\") 失败：%v", err)
	}
	if string(got) != "{}" {
		t.Errorf("默认应返回 {}，实得 %q", got)
	}
	if !json.Valid(got) {
		t.Errorf("返回值应为合法 JSON：%q", got)
	}
}

// 合法的内联 JSON 对象原样通过。
func TestLoadRawConfig_InlineValid(t *testing.T) {
	want := []byte(`{"path":"rl.db","retention_days":7}`)
	got, err := requestlog.LoadRawConfig(string(want), "")
	if err != nil {
		t.Fatalf("合法内联配置应通过，实得错误：%v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("应原样返回内联配置，实得 %q，期望 %q", got, want)
	}
	if !json.Valid(got) {
		t.Errorf("返回值应为合法 JSON：%q", got)
	}
}

// 内联配置非空但不是合法 JSON 对象（首个非空字符不是 '{' 或整体非合法 JSON）时返回错误。
func TestLoadRawConfig_InvalidInline(t *testing.T) {
	cases := []struct{ name, in string }{
		{"array", "[]"},
		{"plain text", "not-json"},
		{"number", "123"},
		{"malformed object", "{bad}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := requestlog.LoadRawConfig(tc.in, "")
			if err == nil {
				t.Fatalf("非法内联配置 %q 应返回错误", tc.in)
			}
			if !strings.Contains(err.Error(), "invalid request log config") {
				t.Errorf("错误信息 %q 未包含 'invalid request log config'", err.Error())
			}
		})
	}
}

// 文件内容不是合法 JSON 对象时同样拒绝。
func TestLoadRawConfig_InvalidFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatalf("写配置文件失败：%v", err)
	}
	if _, err := requestlog.LoadRawConfig("", path); err == nil {
		t.Fatal("文件内容非合法 JSON 对象时应返回错误")
	}
}

// 文件不存在/读失败时返回包装后的底层错误（errors.Is 可拿到 os.ErrNotExist）。
func TestLoadRawConfig_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := requestlog.LoadRawConfig("", missing)
	if err == nil {
		t.Fatal("文件不存在应返回错误")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("错误应包装底层读取错误（os.ErrNotExist），实得 %v", err)
	}
}
