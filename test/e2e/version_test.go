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
//

//go:build e2e

package e2e

// 构建信息用例：公开 GET /version 与两条 aigatectl 入口的契约，
// 并确认 CLI 二进制自身版本与所查询的网关版本被明确区分。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// buildInfo 对齐 GET /version 的响应结构；字段齐全性由 e2e 断言。
type buildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
	StartedAt string `json:"started_at"`
}

// execGatewayFlag 直接执行网关二进制的一个非服务 flag（如 -version），返回退出码与合并输出。
// 它不启动服务、不连数据库，用于把端点的 version 与二进制自报的版本对账。
func execGatewayFlag(t *testing.T, bin string, flag string) (int, string) {
	t.Helper()
	out, err := exec.Command(bin, flag).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("failed to run %s %s: %v", bin, flag, err)
		}
		return exitErr.ExitCode(), string(out)
	}
	return 0, string(out)
}

// TestE2E_Version 覆盖公开构建信息端点：无需鉴权、字段齐全、取值合理，
// 且 /healthz 保持极简未受影响。
func TestE2E_Version(t *testing.T) {
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	gw := startGateway(t, bin, dsn)

	// 1. 不带任何凭据即可读取，且响应是合法 JSON 对象。
	status, body := doRequest(t, http.MethodGet, gw.baseURL+"/version", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /version status = %d, want 200 (body=%q)", status, body)
	}
	var info buildInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatalf("GET /version returned invalid JSON: %v, body=%q", err, body)
	}

	// 2. 六个字段都必须非空；未注入/无 VCS 的构建也应以 unknown 占位而非空串。
	for name, value := range map[string]string{
		"version":    info.Version,
		"commit":     info.Commit,
		"build_time": info.BuildTime,
		"go_version": info.GoVersion,
		"started_at": info.StartedAt,
	} {
		if value == "" {
			t.Errorf("GET /version field %q must never be empty, body=%q", name, body)
		}
	}

	// 3. 运行的确实是刚构建的二进制：版本号必须与 -version 输出一致。
	code, out := execGatewayFlag(t, bin, "-version")
	if code != 0 {
		t.Fatalf("bin/aigate -version exit code = %d, output=%q", code, out)
	}
	if !strings.Contains(out, info.Version) {
		t.Errorf("GET /version version %q not reported by `-version`: %q", info.Version, out)
	}

	// 4. 启动时间必须是不晚于当前时间的 RFC3339 时间戳。
	startedAt, err := time.Parse(time.RFC3339, info.StartedAt)
	if err != nil {
		t.Errorf("started_at %q is not RFC3339: %v", info.StartedAt, err)
	} else if startedAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("started_at %q is in the future", info.StartedAt)
	}

	// 5. 响应体只含构建元数据：不得泄露配置、依赖主机名等侦察信息。
	for _, leaked := range []string{"dsn", "password", "secret", "api_key", upstreamAPIKey} {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(leaked)) {
			t.Errorf("GET /version must not expose %q, body=%q", leaked, body)
		}
	}

	// 6. 健康检查保持极简，版本信息不进 /healthz。
	status, healthBody := doRequest(t, http.MethodGet, gw.baseURL+"/healthz", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", status)
	}
	if !bytes.Equal(bytes.TrimSpace(healthBody), []byte(`{"status":"ok"}`)) {
		t.Errorf("GET /healthz body = %q, want {\"status\":\"ok\"}", healthBody)
	}
}

// TestE2E_CLI_VersionCommands 覆盖 aigatectl 的两条构建信息入口，并区分两个二进制：
//   - `version` 报告 CLI 自身（local binary），不发请求；
//   - `build-info` 与 `version --remote` 报告网关，JSON 与 GET /version 一致；
//     `version --remote` 先打一行标明本地二进制与所查网关地址。
func TestE2E_CLI_VersionCommands(t *testing.T) {
	cliBin := requireEnv(t, envCLIBinKey)
	bin := requireEnv(t, envBinKey)
	dsn := requireEnv(t, envDSNKey)

	gw := startGateway(t, bin, dsn)
	creds := cliEnv{adminToken: adminToken}

	_, rawGateway := doRequest(t, http.MethodGet, gw.baseURL+"/version", "", nil)
	var gateway buildInfo
	if err := json.Unmarshal(rawGateway, &gateway); err != nil {
		t.Fatalf("GET /version returned invalid JSON before CLI checks: %v", err)
	}

	t.Run("version reports the CLI binary locally", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "version")
		if code != 0 {
			t.Fatalf("version exit code = %d (stderr=%q)", code, errOut)
		}
		if len(errOut) != 0 {
			t.Fatalf("version stderr expected empty, got %q", errOut)
		}
		// 必须是本地标识，不能伪装成网关信息。
		if !strings.Contains(string(out), "(local binary)") {
			t.Errorf("version output must flag the local binary, got %q", out)
		}
		if strings.Contains(string(out), gateway.Commit) {
			t.Errorf("version must not print the gateway commit, got %q", out)
		}
	})

	t.Run("build-info reports the gateway", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "build-info")
		cliOK(t, "build-info", code, out, errOut, nil)

		var got buildInfo
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("build-info stdout is not valid JSON: %v, body=%q", err, out)
		}
		if got != gateway {
			t.Errorf("build-info = %+v, want the gateway's %+v", got, gateway)
		}
	})

	t.Run("version --remote names both binaries", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, gw.baseURL, creds, "version", "--remote")
		if code != 0 {
			t.Fatalf("version --remote exit code = %d (stdout=%q stderr=%q)", code, out, errOut)
		}
		if len(errOut) != 0 {
			t.Fatalf("version --remote stderr expected empty, got %q", errOut)
		}

		// 首行是本地二进制标识 + 所查询的网关地址，其余是网关 JSON。
		lines := strings.SplitN(strings.TrimRight(string(out), "\n"), "\n", 2)
		if len(lines) != 2 {
			t.Fatalf("version --remote expected a header line plus JSON, got %q", out)
		}
		if !strings.Contains(lines[0], "(local binary; querying gateway at ") {
			t.Errorf("version --remote header = %q, want local-binary marker with gateway URL", lines[0])
		}
		if !strings.Contains(lines[0], gw.baseURL) {
			t.Errorf("version --remote header = %q, want it to name %q", lines[0], gw.baseURL)
		}

		var got buildInfo
		if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
			t.Fatalf("version --remote payload is not valid JSON: %v, payload=%q", err, lines[1])
		}
		if got != gateway {
			t.Errorf("version --remote payload = %+v, want the gateway's %+v", got, gateway)
		}
	})

	// 与网关失联时必须走传输错误退出码 5，且诊断只写 stderr。
	t.Run("build-info against a dead gateway exits 5", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliBin, deadURL(t), creds, "build-info")
		cliFailure(t, "build-info transport error", 5, code, out, errOut)
	})
}
