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

// 本文件实现内置的 none 实现：不采集、不存储，所有方法都是空操作。
// 它同样注册在表里，使默认配置与其它实现走同一条 Open 代码路径。

import (
	"context"
	"encoding/json"
	"time"
)

func init() {
	Register("none", func() Plugin { return &nonePlugin{} })
}

// nonePlugin 是 no-op 实现。
type nonePlugin struct{}

func (*nonePlugin) Name() string { return "none" }

// Init 忽略配置；none 不需要解析或校验。
func (*nonePlugin) Init(_ context.Context, _ json.RawMessage) error { return nil }

// Write 丢弃批量条目，永不报错。
func (*nonePlugin) Write(_ context.Context, _ []Entry) error { return nil }

// Search 返回非 nil 的空行集合，保证列表序列化为 [] 而非 null。
func (*nonePlugin) Search(_ context.Context, _ Query) (Page, error) {
	return Page{Rows: []Row{}}, nil
}

// Get 永远查不到行。
func (*nonePlugin) Get(_ context.Context, _ string) (*Entry, error) {
	return nil, ErrNotFound
}

// Prune 不删除任何行，返回 0。
func (*nonePlugin) Prune(_ context.Context, _ time.Time) (int64, error) { return 0, nil }

// Retention 返回 0，核心据此不启动清理循环。
func (*nonePlugin) Retention() time.Duration { return 0 }

// Close 无需释放资源。
func (*nonePlugin) Close() error { return nil }
