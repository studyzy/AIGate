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
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"aigate/internal/requestlog"
)

// fakePlugin 是测试用的最小插件实现：记录 Init 收到的原始配置，并可让 Init 返回指定错误。
type fakePlugin struct {
	initRaw json.RawMessage
	initErr error
}

func (p *fakePlugin) Name() string { return "fake" }

func (p *fakePlugin) Init(_ context.Context, raw json.RawMessage) error {
	p.initRaw = raw
	return p.initErr
}

func (p *fakePlugin) Write(_ context.Context, _ []requestlog.Entry) error { return nil }

func (p *fakePlugin) Search(_ context.Context, _ requestlog.Query) (requestlog.Page, error) {
	return requestlog.Page{Rows: []requestlog.Row{}}, nil
}

func (p *fakePlugin) Get(_ context.Context, _ string) (*requestlog.Entry, error) {
	return nil, requestlog.ErrNotFound
}

func (p *fakePlugin) Prune(_ context.Context, _ time.Time) (int64, error) { return 0, nil }

func (p *fakePlugin) Retention() time.Duration { return 0 }

func (p *fakePlugin) Close() error { return nil }

// 编译期断言：fakePlugin 满足 Plugin 契约。
var _ requestlog.Plugin = (*fakePlugin)(nil)

func TestRegister_UnknownName(t *testing.T) {
	_, err := requestlog.Open(context.Background(), "bogus", nil)
	if err == nil {
		t.Fatal("Open 传入未知实现名应返回错误")
	}
	got := err.Error()
	for _, want := range []string{
		`unknown request log implementation "bogus"`,
		"available:",
		"none",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("错误信息 %q 未包含 %q", got, want)
		}
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	const name = "test-duplicate-panics"
	f := func() requestlog.Plugin { return &fakePlugin{} }
	requestlog.Register(name, f) // 首次注册应成功

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("重名注册应 panic")
		}
	}()
	requestlog.Register(name, f) // 同名再次注册应 panic
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("空名注册应 panic")
		}
	}()
	requestlog.Register("", func() requestlog.Plugin { return &fakePlugin{} })
}

func TestNames_Sorted(t *testing.T) {
	names := requestlog.Names()
	found := false
	for _, n := range names {
		if n == "none" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() 未包含内置实现 none：%v", names)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() 未按字典序返回：%v", names)
	}
}

func TestNonePlugin(t *testing.T) {
	ctx := context.Background()
	p, err := requestlog.Open(ctx, "none", nil)
	if err != nil {
		t.Fatalf("Open(none) 失败：%v", err)
	}
	if got := p.Name(); got != "none" {
		t.Errorf("Name() = %q，期望 none", got)
	}
	if err := p.Write(ctx, []requestlog.Entry{{ID: "x"}}); err != nil {
		t.Errorf("Write 应为空操作，实得错误 %v", err)
	}
	if got := p.Retention(); got != 0 {
		t.Errorf("Retention() = %v，期望 0", got)
	}
	page, err := p.Search(ctx, requestlog.Query{})
	if err != nil {
		t.Fatalf("Search 失败：%v", err)
	}
	if page.Rows == nil {
		t.Fatal("Search().Rows 应为非 nil 空切片（返回 [] 而非 null）")
	}
	if len(page.Rows) != 0 {
		t.Errorf("Search().Rows 应为空，实得 %d 行", len(page.Rows))
	}
	if page.Total != 0 {
		t.Errorf("Search().Total = %d，期望 0", page.Total)
	}
	if _, err := p.Get(ctx, "any-id"); !errors.Is(err, requestlog.ErrNotFound) {
		t.Errorf("Get 应返回 ErrNotFound，实得 %v", err)
	}
	n, err := p.Prune(ctx, time.Now())
	if err != nil || n != 0 {
		t.Errorf("Prune 应为空操作：n=%d err=%v", n, err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close 应为空操作：%v", err)
	}
}

func TestOpen_CallsInit(t *testing.T) {
	const name = "test-open-calls-init"
	var created *fakePlugin
	requestlog.Register(name, func() requestlog.Plugin {
		created = &fakePlugin{}
		return created
	})

	raw := json.RawMessage(`{"path":"request-logs.db","retention_days":7}`)
	p, err := requestlog.Open(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("Open 失败：%v", err)
	}
	if p == nil {
		t.Fatal("Open 应返回非 nil 插件")
	}
	if created == nil {
		t.Fatal("factory 未创建插件实例，Init 不可能被调用")
	}
	if !bytes.Equal(created.initRaw, raw) {
		t.Errorf("Init 收到的 raw = %q，期望原样 %q", created.initRaw, raw)
	}
}

func TestOpen_InitErrorPropagates(t *testing.T) {
	const name = "test-open-init-error"
	wantErr := errors.New("init failed")
	requestlog.Register(name, func() requestlog.Plugin {
		return &fakePlugin{initErr: wantErr}
	})

	p, err := requestlog.Open(context.Background(), name, json.RawMessage(`{}`))
	if !errors.Is(err, wantErr) {
		t.Errorf("Open 应把实现的 Init 错误透出，实得 %v", err)
	}
	if p != nil {
		t.Errorf("Init 失败时不应返回插件，实得 %v", p)
	}
}
