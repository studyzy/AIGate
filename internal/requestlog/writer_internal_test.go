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

// 异步批量写入器与保留期清理循环的同包白盒测试。
//
// 通过 fakePlugin 记录 Write 批次、Prune 参数与 Close 调用；通过注入 Writer 的
// batchInterval / pruneInterval 字段（R12）缩短等待，既不改动包级导出常量，
// 也不依赖真实的 2s / 1h 时间尺度。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// fakePlugin 记录每次 Write 的批次副本、每次 Prune 的 before 参数与 Close 次数，
// 并可注入 Write/Prune 错误、保留期以及一个可选的 Write 阻塞门（用于构造排空超时）。
type fakePlugin struct {
	mu          sync.Mutex
	batches     [][]Entry
	pruneBefore []time.Time
	closes      int

	writeErr  error
	pruneErr  error
	retention time.Duration

	// writeGate 非 nil 时，Write 先阻塞在此通道上（在加锁之前），使排空无法完成，
	// 用于验证 ctx 放行后仍调用 Close（R13）。该字段只在构造时设置，之后不再修改。
	writeGate chan struct{}
}

var _ Plugin = (*fakePlugin)(nil)

func (p *fakePlugin) Name() string { return "fake" }

func (p *fakePlugin) Init(_ context.Context, _ json.RawMessage) error { return nil }

func (p *fakePlugin) Write(_ context.Context, entries []Entry) error {
	if p.writeGate != nil {
		<-p.writeGate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, append([]Entry(nil), entries...))
	return p.writeErr
}

func (p *fakePlugin) Search(_ context.Context, _ Query) (Page, error) {
	return Page{Rows: []Row{}}, nil
}

func (p *fakePlugin) Get(_ context.Context, _ string) (*Entry, error) {
	return nil, ErrNotFound
}

func (p *fakePlugin) Prune(_ context.Context, before time.Time) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneBefore = append(p.pruneBefore, before)
	return 0, p.pruneErr
}

func (p *fakePlugin) Retention() time.Duration { return p.retention }

func (p *fakePlugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

// batchSnapshot 返回各次 Write 批次的副本。
func (p *fakePlugin) batchSnapshot() [][]Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]Entry, len(p.batches))
	for i := range p.batches {
		out[i] = append([]Entry(nil), p.batches[i]...)
	}
	return out
}

// totalEntries 返回所有 Write 批次的条目总数。
func (p *fakePlugin) totalEntries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, b := range p.batches {
		n += len(b)
	}
	return n
}

// pruneSnapshot 返回 Prune 调用次数与各次 before 参数副本。
func (p *fakePlugin) pruneSnapshot() (int, []time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	before := append([]time.Time(nil), p.pruneBefore...)
	return len(before), before
}

// closeCount 返回 Close 调用次数。
func (p *fakePlugin) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

// newWriter 构造写入器并把可注入周期改为「超长」，默认关闭定时 flush / 定时清理，
// 由用例按需显式缩短某一字段。这样断言只由被测行为（批次大小、启动清理、排空）驱动。
func newWriter(p *fakePlugin) *Writer {
	w := NewWriter(p)
	w.batchInterval = time.Hour
	w.pruneInterval = time.Hour
	return w
}

// waitFor 轮询 probe 直到返回 nil 或超时。
func waitFor(t *testing.T, desc string, timeout time.Duration, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = probe()
		if lastErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timeout after %s, last error: %v", desc, timeout, lastErr)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitEntries 等待累计写入条目数达到 want。
func waitEntries(t *testing.T, p *fakePlugin, want int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("total entries >= %d", want), 2*time.Second, func() error {
		if got := p.totalEntries(); got < want {
			return fmt.Errorf("total=%d, want >=%d", got, want)
		}
		return nil
	})
}

// submitN 投递 n 条空 Entry。
func submitN(w *Writer, n int) {
	for i := 0; i < n; i++ {
		w.Submit(Entry{})
	}
}

// lens 返回各批次长度，仅用于失败信息。
func lens(b [][]Entry) []int {
	out := make([]int, len(b))
	for i := range b {
		out[i] = len(b[i])
	}
	return out
}

// ---------------------------------------------------------------------------
// slog.Warn 计数夹具
// ---------------------------------------------------------------------------

// warnCounter 统计 slog.Warn 级日志条数（并发安全）。
type warnCounter struct {
	n atomic.Int64
}

func (*warnCounter) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (c *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		c.n.Add(1)
	}
	return nil
}

func (c *warnCounter) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *warnCounter) WithGroup(_ string) slog.Handler      { return c }

func (c *warnCounter) count() int64 { return c.n.Load() }

// installWarnCounter 临时替换默认 logger 以统计 Warn，测试结束自动还原。
func installWarnCounter(t *testing.T) *warnCounter {
	t.Helper()
	c := &warnCounter{}
	old := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(old) })
	return c
}

// ---------------------------------------------------------------------------
// 常量与默认值（R12）
// ---------------------------------------------------------------------------

// TestWriter_Defaults 固定导出常量的精确取值，并验证 NewWriter 把可注入字段
// 初始化为对应常量（R12：常量保持 const，测试只改字段）。
func TestWriter_Defaults(t *testing.T) {
	if QueueCapacity != 256 {
		t.Errorf("QueueCapacity = %d, want 256", QueueCapacity)
	}
	if BatchSize != 64 {
		t.Errorf("BatchSize = %d, want 64", BatchSize)
	}
	if BatchInterval != 2*time.Second {
		t.Errorf("BatchInterval = %v, want 2s", BatchInterval)
	}
	if PruneInterval != time.Hour {
		t.Errorf("PruneInterval = %v, want 1h", PruneInterval)
	}
	w := NewWriter(&fakePlugin{})
	if w.batchInterval != BatchInterval {
		t.Errorf("batchInterval = %v, want %v", w.batchInterval, BatchInterval)
	}
	if w.pruneInterval != PruneInterval {
		t.Errorf("pruneInterval = %v, want %v", w.pruneInterval, PruneInterval)
	}
	if got := cap(w.ch); got != QueueCapacity {
		t.Errorf("channel capacity = %d, want %d", got, QueueCapacity)
	}
}

// ---------------------------------------------------------------------------
// 批量写入
// ---------------------------------------------------------------------------

// TestWriter_BatchBySize：投满 BatchSize 条触发一次 Write，且一次收到全部条目。
func TestWriter_BatchBySize(t *testing.T) {
	f := &fakePlugin{}
	w := newWriter(f)
	w.Start()
	t.Cleanup(func() { w.Stop(context.Background()) })

	submitN(w, BatchSize)

	waitEntries(t, f, BatchSize)

	batches := f.batchSnapshot()
	if len(batches) != 1 || len(batches[0]) != BatchSize {
		t.Fatalf("want 1 Write of %d entries, got batches=%v", BatchSize, lens(batches))
	}
	if got := w.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0", got)
	}
}

// TestWriter_BatchByInterval：投 1 条（不足 BatchSize），缩短 batchInterval 后
// 由定时触发 Write，且恰好收到 1 条。
func TestWriter_BatchByInterval(t *testing.T) {
	f := &fakePlugin{}
	w := newWriter(f)
	w.batchInterval = 20 * time.Millisecond
	w.Start()
	t.Cleanup(func() { w.Stop(context.Background()) })

	submitN(w, 1)
	waitEntries(t, f, 1)

	batches := f.batchSnapshot()
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Fatalf("want 1 Write of 1 entry after BatchInterval, got batches=%v", lens(batches))
	}
}

// TestWriter_QueueFullDrops：塞满 QueueCapacity 条后再投，应丢弃、计数、
// slog.Warn 且不阻塞（用带超时的 goroutine 断言 Submit 立即返回）。
func TestWriter_QueueFullDrops(t *testing.T) {
	c := installWarnCounter(t)
	f := &fakePlugin{}
	w := newWriter(f) // 不 Start：consumer 不消费，队列可被填满

	submitN(w, QueueCapacity)

	returned := make(chan struct{})
	go func() {
		w.Submit(Entry{}) // 第 QueueCapacity+1 条
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("queue full: Submit must not block")
	}

	if got := w.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
	if got := c.count(); got != 1 {
		t.Errorf("slog.Warn count = %d, want 1", got)
	}
	if got := f.totalEntries(); got != 0 {
		t.Errorf("no consumer started: total written = %d, want 0", got)
	}
}

// TestWriter_WriteErrorDoesNotBlock：fake Write 返回 error 时，后续批次仍被处理，
// 不重试（每批恰好一次），且错误只以 slog.Warn 记录。
func TestWriter_WriteErrorDoesNotBlock(t *testing.T) {
	c := installWarnCounter(t)
	f := &fakePlugin{writeErr: errors.New("write failed")}
	w := newWriter(f)
	w.Start()
	t.Cleanup(func() { w.Stop(context.Background()) })

	submitN(w, BatchSize)
	waitEntries(t, f, BatchSize)
	submitN(w, BatchSize)
	waitEntries(t, f, 2*BatchSize)

	batches := f.batchSnapshot()
	if len(batches) != 2 {
		t.Fatalf("want 2 Write calls (no retry), got %d: %v", len(batches), lens(batches))
	}
	if got := f.totalEntries(); got != 2*BatchSize {
		t.Errorf("total written = %d, want %d", got, 2*BatchSize)
	}
	waitFor(t, "write errors warned", 2*time.Second, func() error {
		if c.count() < 2 {
			return fmt.Errorf("warn count=%d, want >=2", c.count())
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 保留期清理
// ---------------------------------------------------------------------------

// TestWriter_ZeroRetentionNoPrune（Review Focus 5）：Retention() <= 0 时
// Start() 不启动清理循环、不产生任何 Prune 调用，即使等待超过注入的清理周期。
func TestWriter_ZeroRetentionNoPrune(t *testing.T) {
	for _, retention := range []time.Duration{0, -time.Hour} {
		t.Run(fmt.Sprintf("retention=%v", retention), func(t *testing.T) {
			f := &fakePlugin{retention: retention}
			w := newWriter(f)
			w.pruneInterval = 10 * time.Millisecond // 注入可缩短的清理周期
			w.Start()
			t.Cleanup(func() { w.Stop(context.Background()) })

			time.Sleep(60 * time.Millisecond) // > pruneInterval

			n, _ := f.pruneSnapshot()
			if n != 0 {
				t.Fatalf("Retention()<=0 must make ZERO Prune calls, got %d", n)
			}
		})
	}
}

// TestWriter_PruneWhenConfigured：Retention()==7d 时启动即同步清一次，
// before ≈ now-7d（UTC）；此后按 pruneInterval 周期性清理。
func TestWriter_PruneWhenConfigured(t *testing.T) {
	const retention = 7 * 24 * time.Hour
	f := &fakePlugin{retention: retention}
	w := newWriter(f)
	w.pruneInterval = 20 * time.Millisecond
	start := time.Now().UTC()
	w.Start()
	t.Cleanup(func() { w.Stop(context.Background()) })

	// 启动即同步清一次：Start() 返回后应至少有一次 Prune。调度较慢时清理循环可能
	// 已经又 tick 了一次，所以不能断言恰好 1 次（T2，避免慢调度下的偶发抖动）。
	n, before := f.pruneSnapshot()
	if n < 1 {
		t.Fatalf("startup inline Prune: got %d calls, want >=1", n)
	}
	wantBefore := start.Add(-retention)
	if d := before[0].Sub(wantBefore); d > 2*time.Second || d < -2*time.Second {
		t.Errorf("Prune before = %v, want ~%v (now-7d)", before[0], wantBefore)
	}
	if before[0].Location() != time.UTC {
		t.Errorf("Prune before 应为 UTC，实得 %v", before[0].Location())
	}

	// 周期性清理：出现第二次 Prune，证明清理循环在运行。
	waitFor(t, "periodic prune", 2*time.Second, func() error {
		if n, _ := f.pruneSnapshot(); n < 2 {
			return fmt.Errorf("prune calls=%d, want >=2", n)
		}
		return nil
	})
}

// TestWriter_PruneErrorDoesNotStopLoop：Prune 报错只记 slog.Warn，不终止清理循环。
func TestWriter_PruneErrorDoesNotStopLoop(t *testing.T) {
	c := installWarnCounter(t)
	f := &fakePlugin{retention: time.Hour, pruneErr: errors.New("prune failed")}
	w := newWriter(f)
	w.pruneInterval = 20 * time.Millisecond
	w.Start()
	t.Cleanup(func() { w.Stop(context.Background()) })

	waitFor(t, "prune keeps looping after error", 2*time.Second, func() error {
		if n, _ := f.pruneSnapshot(); n < 2 {
			return fmt.Errorf("prune calls=%d, want >=2", n)
		}
		return nil
	})
	waitFor(t, "prune errors warned", 2*time.Second, func() error {
		if c.count() < 2 {
			return fmt.Errorf("warn count=%d, want >=2", c.count())
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// 停机排空（R13）
// ---------------------------------------------------------------------------

// TestWriter_StopDrainsAndCloses：Stop(ctx) 后剩余条目全部落库、Close 被调用且只调用一次；
// ctx 已取消时 Stop 立即返回，且即便排空被放行（超时分支）仍须调用 Close（R13）。
func TestWriter_StopDrainsAndCloses(t *testing.T) {
	t.Run("drains and closes", func(t *testing.T) {
		f := &fakePlugin{}
		w := newWriter(f)
		w.Start()
		w.Start() // Start 幂等：不应重复启动 consumer

		const n = 5
		submitN(w, n) // 不足 BatchSize 且定时 flush 关闭，条目留到排空

		w.Stop(context.Background())
		w.Stop(context.Background()) // Stop 幂等：不应 panic，Close 仍只调用一次

		if got := f.totalEntries(); got != n {
			t.Errorf("after Stop: written = %d, want %d", got, n)
		}
		if got := w.Dropped(); got != 0 {
			t.Errorf("Dropped() = %d, want 0", got)
		}
		if got := f.closeCount(); got != 1 {
			t.Errorf("Close() = %d, want 1", got)
		}
	})

	t.Run("cancelled ctx returns immediately and still closes", func(t *testing.T) {
		f := &fakePlugin{writeGate: make(chan struct{})}
		w := newWriter(f)
		w.Start()
		// cleanup：释放被阻塞的 Write，避免 consumer goroutine 永久挂住。
		t.Cleanup(func() { close(f.writeGate) })

		submitN(w, 1) // 留一条待排空，使 consumer 的 Write 阻塞在 gate 上

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		returned := make(chan struct{})
		go func() {
			w.Stop(ctx)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("cancelled ctx: Stop must return immediately")
		}

		// R13：即使排空被 ctx 放行，Close 仍须被调用，否则泄漏插件的 DB/文件句柄。
		if got := f.closeCount(); got != 1 {
			t.Errorf("Close() = %d, want 1 (must close even on drain timeout)", got)
		}
	})

	t.Run("submit after stop does not panic", func(t *testing.T) {
		f := &fakePlugin{}
		w := newWriter(f)
		w.Start()
		w.Stop(context.Background())

		before := w.Dropped()
		submitN(w, 3) // 停机后的投递不得 panic（send on closed channel），只计入丢弃
		if got := w.Dropped() - before; got != 3 {
			t.Errorf("dropped after Stop = %d, want 3", got)
		}
	})
}
