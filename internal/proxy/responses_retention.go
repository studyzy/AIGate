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

package proxy

// 本文件实现 Responses 有状态能力的装配与生命周期（design §5.6）：
//   - SetResponseStorage：站点级存储开关（-allow-response-storage）；
//   - StartResponseRetention / StopResponseRetention：过期行清理循环的启停，
//     形态照 event/dispatcher.go 的 pruneLoop（启动先清一次 + 每小时一次 + 失败只 slog.Warn）。
//
// 有状态请求的读写逻辑在 responses_state.go，这里只负责装配。

import (
	"context"
	"log/slog"
	"time"
)

// responsesPruneInterval 是过期会话状态的清理周期（与 webhook 投递记录清理一致，每小时一次）。
const responsesPruneInterval = time.Hour

// SetResponseStorage 配置 Responses 站点级存储开关（-allow-response-storage）。
//
// allow 为 false 时：数据面对所有 /v1/responses 请求按 store:false 处理；显式 store:true、
// previous_response_id、conversation（以及后续的 background）一律 400——明确拒绝而非静默改写。
//
// NewHandler 已按默认值（true）初始化本开关，装配层只在关闭时调用本方法即可。
// 返回 error 只为将来可能的装配期校验留扩展点（当前没有可失败步骤），调用方仍按可失败处理。
func (h *Handler) SetResponseStorage(allow bool) error {
	h.allowResponseStorage = allow
	return nil
}

// StartResponseRetention 在 retention > 0 时启动过期会话状态清理循环，
// ≤ 0 表示永不清理（-response-retention-days=0），直接返回。重复调用为空操作。
func (h *Handler) StartResponseRetention(retention time.Duration) {
	h.pruneStartOnce.Do(func() {
		if retention <= 0 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.pruneCancel = cancel
		go h.pruneLoop(ctx, retention)
	})
}

// StopResponseRetention 让清理循环退出（幂等；未启动或保留期为 0 时为空操作）。
// 优雅停机不需要等待它——它不持有请求级资源。
func (h *Handler) StopResponseRetention() {
	if h.pruneCancel != nil {
		h.pruneCancel()
	}
}

// pruneLoop 周期性清理过期会话状态：启动时先清一次（覆盖停机期间累积的旧行），
// 之后每 responsesPruneInterval 一次，直到 ctx 被取消。
func (h *Handler) pruneLoop(ctx context.Context, retention time.Duration) {
	h.pruneResponseStates(retention)
	ticker := time.NewTicker(responsesPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.pruneResponseStates(retention)
		case <-ctx.Done():
			return
		}
	}
}

// pruneResponseStates 删除创建时间早于「现在 − 保留期」的行；失败只记 Warn（不中断循环）。
func (h *Handler) pruneResponseStates(retention time.Duration) {
	before := time.Now().UTC().Add(-retention)
	n, err := h.store.PruneResponseStates(context.Background(), before)
	if err != nil {
		slog.Warn("prune response states failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("pruned response states", "rows", n, "retention", retention.String())
	}
}
