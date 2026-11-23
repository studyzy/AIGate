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

// 网关进程夹具：在被测端口池内挑一个空闲端口启动真实的 bin/aigate，
// 等 /healthz 就绪，并在 t.Cleanup 中优雅停止。

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer 是并发安全的进程输出缓冲：主 goroutine 可随时读它拼装失败信息，
// 而子进程仍在后台写入。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type gatewayProcess struct {
	cmd     *exec.Cmd
	baseURL string
	out     *syncBuffer
}

// freePort 申请一个空闲的环回端口后立刻释放，交给被测进程使用，避免端口写死。
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// gatewayPort 从 e2e 端口池（gatewayPortMin..gatewayPortMax）里随机取一个空闲端口：每次运行都不同，
// 且绝不使用 8080 等默认端口，避免与本机常驻的 AIGate 服务冲突。
func gatewayPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", gatewayPortMin))
	if err != nil {
		t.Fatalf("failed to allocate a free port from the e2e port pool: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startGateway(t *testing.T, bin, dsn string) *gatewayProcess {
	t.Helper()
	return startGatewayAt(t, bin, dsn, gatewayPort(t))
}

// startGatewayAt 在指定端口启动网关。自定义端口必须落在端口池内：绝不能落到 8080，否则会与
// 本机常驻的 AIGate 服务抢端口，用例可能打到别人的实例上。
func startGatewayAt(t *testing.T, bin, dsn string, port int) *gatewayProcess {
	t.Helper()
	if port < gatewayPortMin || port > gatewayPortMax {
		t.Fatalf("gateway port %d is outside the e2e port pool [%d, %d]", port, gatewayPortMin, gatewayPortMax)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	out := &syncBuffer{}
	cmd := exec.Command(bin,
		"-addr", addr,
		"-db-type", dbType(),
		"-db-dsn", dsn,
		"-timezone", "Asia/Shanghai",
		"-usage-buffer", "256",
		"-quota-exceeded-status", "429",
		"-init-admin-token", adminToken,
		// 注意：-allow-private-upstreams 是布尔 flag，必须用 =true 形式；
		// 若写成「-allow-private-upstreams true」，Go flag 会把 true 当作位置参数并停止解析，
		// 之后的 flag（如 -master-key）会被静默忽略。
		"-allow-private-upstreams=true",
		"-master-key", e2eMasterKey,
	)
	// 隔离监听与库相关环境变量：即使外部 export 了 AIGATE_ADDR（例如指向 8080）也不会覆盖 -addr；
	// AIGATE_DB_DSN 则作为进程环境标记，供 run.sh 的兜底清理精确定位仍连着测试库的网关进程。
	cmd.Env = append(os.Environ(), "AIGATE_ADDR="+addr, "AIGATE_DB_DSN="+dsn)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start gateway process (bin=%s): %v", bin, err)
	}

	gp := &gatewayProcess{cmd: cmd, baseURL: "http://" + addr, out: out}
	t.Cleanup(gp.stop)

	gp.waitHealthy(t)
	return gp
}

// stop 向子进程发 SIGTERM 并等待退出；超时后强杀，避免残留进程占用随机端口。
func (g *gatewayProcess) stop() {
	if g.cmd.Process == nil {
		return
	}
	_ = g.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- g.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = g.cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(stopTimeout):
			// Wait 迟迟不返回时不阻塞清理：进程已被 Kill，交给用例结束后统一兜底回收。
		}
	}
}

// waitHealthy 轮询 GET /healthz 直到 200；超时则打印子进程输出并终止用例。
func (g *gatewayProcess) waitHealthy(t *testing.T) {
	t.Helper()

	url := g.baseURL + "/healthz"
	deadline := time.Now().Add(healthPollTimeout)
	var lastStatus int
	var lastErr error

	for {
		status, body, err := tryGet(url)
		switch {
		case err == nil && status == http.StatusOK:
			return
		case err == nil:
			lastStatus, lastErr = status, fmt.Errorf("response body: %s", body)
		default:
			lastErr = err
		}

		if time.Now().After(deadline) {
			t.Fatalf(
				"timed out waiting for gateway /healthz to become ready (%s, last status=%d, error=%v)\n--- gateway subprocess "+
					"output ---\n%s",
				healthPollTimeout,
				lastStatus,
				lastErr,
				g.out.String(),
			)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func tryGet(url string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := e2eHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}
