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

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"aigate/internal/billing"
	"aigate/internal/model"
)

// UsageWriter 用量异步写入器：转发路径只投递，不阻塞请求。
// 有界队列满时丢弃并告警（网关不承担计费真源职责，容忍极低概率丢失）。
//
// 计价同样在消费侧完成：数据面从不调用计价引擎，因此价目/汇率/日历查询（可能触达
// 数据库）永远不会拖慢请求。代价是金额在落库前才定，与 entry.CreatedAt 口径一致。
type UsageWriter struct {
	store   *model.Store
	biller  *billing.Engine
	ch      chan model.LLMUsageLog
	done    chan struct{}
	dropped atomic.Int64

	startOnce sync.Once
	stopOnce  sync.Once

	// mu/closed 保护「Submit 发送」与「Stop 关闭」的竞争：
	// Submit 持读锁发送，Stop 持写锁置位并 close(ch)，避免 send on closed channel。
	mu     sync.RWMutex
	closed bool
}

// NewUsageWriter 创建写入器（buffer < 1 时使用 256）。
// biller 为 nil 时跳过计价：金额列保持 0，日志行照常落库（未装配计价的调用方不受影响）。
func NewUsageWriter(store *model.Store, buffer int, biller *billing.Engine) *UsageWriter {
	if buffer < 1 {
		buffer = 256
	}
	return &UsageWriter{
		store:  store,
		biller: biller,
		ch:     make(chan model.LLMUsageLog, buffer),
		done:   make(chan struct{}),
	}
}

// Start 启动消费 goroutine（重复调用为空操作）。
func (w *UsageWriter) Start() {
	w.startOnce.Do(func() {
		go func() {
			defer close(w.done)
			ctx := context.Background()
			for entry := range w.ch {
				w.bill(ctx, &entry)
				if err := w.store.InsertUsageLog(ctx, &entry); err != nil {
					slog.Error("write usage log failed", "error", err)
				}
				if err := w.store.UpsertDailyUsage(ctx, &entry); err != nil {
					slog.Error("upsert daily usage failed", "error", err)
				}
			}
		}()
	})
}

// bill 在落库前就地补齐 entry 的两个金额列与计价币种。
//
// 记账完整性：计价失败也绝不丢行——引擎在仅缺汇率时仍返回模型币种金额，
// 原样落库后只告警平台金额记 0；完全没有生效价目时才两列都记 0。
// 未装配 biller 时直接返回，行为与引入计价前一致。
func (w *UsageWriter) bill(ctx context.Context, entry *model.LLMUsageLog) {
	if w.biller == nil {
		return
	}
	res, err := w.biller.Compute(ctx, entry.AIModelID, billing.Input{
		At:           entry.CreatedAt,
		Tokens:       billing.ToTokens(entry.PromptCacheReadTokens, entry.PromptCacheWriteTokens, entry.PromptTokens, entry.CompletionTokens),
		ReportedCost: entry.ReportedCost,
	})
	if err != nil {
		slog.Warn("usage billing failed, storing zero cost",
			"error", err, "ai_model_id", entry.AIModelID, "api_key_id", entry.APIKeyID)
	}
	// 失败时 Result 可能部分有效：保留引擎已算出的模型币种金额，平台金额按结果取值。
	entry.CostVendorMicros = res.VendorMicros
	entry.CostPlatformMicros = res.PlatformMicros
	entry.Currency = res.ModelCurrency
}

// Submit 投递一条用量日志；队列满或已关闭时丢弃并计数。
func (w *UsageWriter) Submit(entry model.LLMUsageLog) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- entry:
	default:
		n := w.dropped.Add(1)
		slog.Warn("usage log queue full, dropping entry", "dropped_total", n)
	}
}

// Stop 关闭队列并等待排空；ctx 到期仍未排空时直接返回。重复调用为空操作。
func (w *UsageWriter) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.ch)
		w.mu.Unlock()
		w.Start()
		select {
		case <-w.done:
		case <-ctx.Done():
		}
	})
}

// Dropped 返回累计丢弃条数。
func (w *UsageWriter) Dropped() int64 {
	return w.dropped.Load()
}
