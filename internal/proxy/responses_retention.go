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
// ≤ 0 表示永不清理（-response-retention-days=0），此时只剩「重启遗留行」检查。
// 重复调用为空操作。
//
// 为什么保留期为 0 也要启动：受保护的合法任务不会老过超时（每周期刷新 updated_at），
// 「把老过后台超时的在途行标记为 failed」因此与保留期无关，见 startResponseSweep。
func (h *Handler) StartResponseRetention(retention time.Duration) {
	h.pruneStartOnce.Do(func() {
		// 两个循环共用一个取消句柄（保留期循环在「启动时先清一次」后进入睡眠，
		// 不能让它被残留扫描覆盖，否则 StopResponseRetention 停不掉它）。
		//
		// 保留期的启动清理在**本函数内联**执行，保留期 > 0 时必然发生（不依赖任何
		// goroutine 的调度时序）；pruneLoop 只负责其后的周期清理。
		ctx, cancel := context.WithCancel(context.Background())
		h.pruneCancel = cancel
		if retention > 0 {
			h.pruneResponseStates(retention)
			go h.pruneLoop(ctx, retention)
		}
		startResponseSweep(ctx, h, retention)
	})
}

// StopResponseRetention 让清理循环退出（幂等；未启动或保留期为 0 时为空操作）。
// 优雅停机不需要等待它——它不持有请求级资源。
func (h *Handler) StopResponseRetention() {
	if h.pruneCancel != nil {
		h.pruneCancel()
	}
}

// pruneLoop 周期性清理过期会话状态：每 responsesPruneInterval 一次，直到 ctx 被取消。
// 启动时的那次清理由 StartResponseRetention 内联完成（见那里的顺序说明）。
func (h *Handler) pruneLoop(ctx context.Context, retention time.Duration) {
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

// startResponseSweep 启动「重启遗留行」检查循环（与保留期互相独立，见 StartResponseRetention）。
//
// 启动后的首个检查在 responseBackgroundSweepStartDelay 之后执行，而不是立刻执行：
// 清理循环在 Register 之前装配，此刻库里的在途行**可能正是本进程即将收尾的任务**
// （例如同一进程内先起服务再被外部探活触发的短暂重叠）。延迟一秒并用「老过后台超时」
// 判定，两者叠加后误伤一个本实例在途任务所需的时间窗远大于后台超时本身。
// 之后每分钟检查一次，让可观测残留窗口从「保留期循环的 1 小时」压到分钟级。
func startResponseSweep(ctx context.Context, h *Handler, retention time.Duration) {
	go h.backgroundSweepLoop(ctx, retention)
}

// backgroundSweepLoop 周期性地把重启遗留的在途行标记为 failed。
// 保留期有效（> 0）时顺带清理过期行，形态与 pruneLoop 一致。
func (h *Handler) backgroundSweepLoop(ctx context.Context, retention time.Duration) {
	select {
	case <-time.After(responseBackgroundSweepStartDelay):
	case <-ctx.Done():
		return
	}
	sweep := func() {
		if n := h.staleResponseStates(ctx, time.Now()); n > 0 && retention > 0 {
			// 与清理同一节奏：残留检查不负责删除，但保留期有效时顺带触发一次，
			// 保证 startup 之后新出现的过期行也会在分钟级被清掉（pruneLoop 仍是主清理者）。
			h.pruneResponseStates(retention)
		}
	}
	sweep()
	ticker := time.NewTicker(responseBackgroundSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			sweep()
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
