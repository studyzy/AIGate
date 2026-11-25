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

package requestlog

// 本文件实现请求日志的异步写入器与保留期清理循环：有界队列 + 单 consumer +
// 满即丢 + 按批次/周期刷写。数据面 Middleware 在请求路径上同步调用 Submit，
// 因此 Submit 绝不阻塞、绝不 panic；落库失败只告警，绝不影响数据面。

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// QueueCapacity 是待写队列容量；满即丢弃（容忍极低概率丢失，不拖慢请求）。
	QueueCapacity = 256
	// BatchSize 是单次 Write 的最大条目数，达到即刷写。
	BatchSize = 64
	// BatchInterval 是未凑满一批时的最长等待，到点刷写当前批次。
	BatchInterval = 2 * time.Second
	// PruneInterval 是保留期清理的周期。
	PruneInterval = time.Hour
)

// Writer 有界队列 + 单 consumer 的异步批量写入器，实现 Sink。
//
// 队列满时 Submit 丢弃并计数（不阻塞调用方）；consumer 按 BatchSize 或
// BatchInterval 触发一次 Plugin.Write，失败只记告警、不重试、不中断消费。
// Retention() > 0 时另起清理循环，启动即清一次、此后每 PruneInterval 清理一次。
type Writer struct {
	p       Plugin
	ch      chan Entry
	done    chan struct{}
	dropped atomic.Int64

	// batchInterval / pruneInterval 允许测试注入更短的周期（生产由同名常量初始化），
	// 避免改动包级导出常量、也避免测试间共享全局状态（R12）。
	batchInterval time.Duration
	pruneInterval time.Duration

	startOnce sync.Once
	stopOnce  sync.Once

	// mu/closed 保护「Submit 发送」与「Stop 关闭」的竞争：
	// Submit 持读锁发送，Stop 持写锁置位并 close(ch)，避免 send on closed channel。
	mu     sync.RWMutex
	closed bool
}

// 编译期断言：Writer 必须实现 Sink（Task 3 中间件在请求路径上同步调用 Submit）。
var _ Sink = (*Writer)(nil)

// NewWriter 创建写入器；消费与清理循环由 Start 启动。
func NewWriter(p Plugin) *Writer {
	return &Writer{
		p:             p,
		ch:            make(chan Entry, QueueCapacity),
		done:          make(chan struct{}),
		batchInterval: BatchInterval,
		pruneInterval: PruneInterval,
	}
}

// Submit 投递一条日志并实现 Sink：队列满或已关闭时丢弃并计数，绝不阻塞。
func (w *Writer) Submit(e Entry) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- e:
	default:
		n := w.dropped.Add(1)
		slog.Warn("request log queue full, dropping entry", "dropped_total", n)
	}
}

// Dropped 返回累计丢弃条数。
func (w *Writer) Dropped() int64 {
	return w.dropped.Load()
}

// Start 启动消费 goroutine（重复调用为空操作）；配置了保留期时同步清理一次
// 并启动周期性清理循环。
func (w *Writer) Start() {
	w.startOnce.Do(func() {
		go w.loop()
		if retention := w.p.Retention(); retention > 0 {
			w.pruneOnce(retention) // 启动即清一次，覆盖停机期间累积的旧记录
			go w.pruneLoop(retention)
		}
	})
}

// Stop 关闭队列并等待排空；ctx 到期仍未排空时放行。重复调用为空操作。
// 无论排空完成还是 ctx 放行，都在排空 select 结束后调用 p.Close()：
// 既不与排空并发，也不因超时跳过（否则泄漏插件的 DB/文件句柄，R13）。
func (w *Writer) Stop(ctx context.Context) {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.ch)
		w.mu.Unlock()
		w.Start() // 保证 consumer 一定运行、channel 一定排空、done 一定关闭
		select {
		case <-w.done:
		case <-ctx.Done():
		}
		if err := w.p.Close(); err != nil {
			slog.Warn("request log close plugin failed", "error", err)
		}
	})
}

// loop 是唯一 consumer：按批次或周期刷写，channel 关闭后清空剩余并返回。
func (w *Writer) loop() {
	defer close(w.done)
	ctx := context.Background()
	batch := make([]Entry, 0, BatchSize)
	ticker := time.NewTicker(w.batchInterval)
	defer ticker.Stop()

	// flush 把当前批次交给插件；失败只告警、不重试、不中断消费。
	// 刷写后换一个新切片，保证交给插件的批次不会被后续 append 覆盖。
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.p.Write(ctx, batch); err != nil {
			slog.Warn("request log write batch failed", "error", err, "entries", len(batch))
		}
		batch = make([]Entry, 0, BatchSize)
	}

	for {
		select {
		case e, ok := <-w.ch:
			if !ok {
				flush() // 排空：提交最后不足一批的条目
				return
			}
			batch = append(batch, e)
			if len(batch) >= BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// pruneLoop 周期性清理过期记录，队列排空（done 关闭）后退出。
func (w *Writer) pruneLoop(retention time.Duration) {
	ticker := time.NewTicker(w.pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.pruneOnce(retention)
		case <-w.done:
			return
		}
	}
}

// pruneOnce 清理创建时间早于「现在 − 保留期」的记录；失败只记告警。
func (w *Writer) pruneOnce(retention time.Duration) {
	before := time.Now().UTC().Add(-retention)
	if _, err := w.p.Prune(context.Background(), before); err != nil {
		slog.Warn("request log prune failed", "error", err)
	}
}
