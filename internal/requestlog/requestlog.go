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

// Package requestlog 定义请求日志能力的插件契约、具名注册表与选择器。
//
// 核心只认 Plugin 接口、注册表与 Open 选择器；表结构、连接、索引、配置项与校验
// 全部由实现自带。契约文件只依赖标准库，不 import internal/model 或 gorm；新增实现
// 通过 init 调用 Register 注册到同一包内，因此只需在本包加文件，无需改动核心。
package requestlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound 表示按 ID 查询不到对应的请求日志行。
var ErrNotFound = errors.New("request log not found")

// Entry 是一次 LLM 请求/响应的完整记录：详情接口返回它，含请求体、响应体与白名单头。
type Entry struct {
	ID                                     string
	CreatedAt                              time.Time
	Method, Path, Model, APIKeyHash        string
	StatusCode, LatencyMS                  int
	Stream                                 bool
	RequestBody, ResponseBody              string
	RequestHeaders, ResponseHeaders, Error string
	Truncated                              bool
}

// Row 是列表行：字段与 Entry 相同，但去掉四个大字段
// （RequestBody / ResponseBody / RequestHeaders / ResponseHeaders），避免列表接口回带正文。
type Row struct {
	ID                              string
	CreatedAt                       time.Time
	Method, Path, Model, APIKeyHash string
	StatusCode, LatencyMS           int
	Stream                          bool
	Error                           string
	Truncated                       bool
}

// Query 是列表查询的过滤与分页条件；0 / 空串 / nil 均表示不过滤该维度。
type Query struct {
	Start, End                 *time.Time
	APIKeyHash, Model, Path    string
	StatusCode                 int
	Stream                     *bool
	MinLatencyMS, MaxLatencyMS int
	Q                          string
	Page, PageSize             int
}

// Page 是列表结果：Rows 为当前页行，Total 为匹配总数，Page/PageSize 回显分页。
type Page struct {
	Rows     []Row
	Total    int64
	Page     int
	PageSize int
}

// Plugin 是请求日志存储的完整能力：装配期 Init、批量写入、读取、清理与关闭。
type Plugin interface {
	Name() string
	Init(ctx context.Context, raw json.RawMessage) error
	Write(ctx context.Context, entries []Entry) error
	Search(ctx context.Context, q Query) (Page, error)
	Get(ctx context.Context, id string) (*Entry, error)
	Prune(ctx context.Context, before time.Time) (int64, error)
	Retention() time.Duration
	Close() error
}

// Reader 是管理面依赖的窄接口，只暴露读取能力。
type Reader interface {
	Search(ctx context.Context, q Query) (Page, error)
	Get(ctx context.Context, id string) (*Entry, error)
}

// Factory 创建某个实现的一个新实例，供注册表保存。
type Factory func() Plugin

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register 注册一个实现，由各实现的 init 调用；空名或重名直接 panic（同 registerStrategy）。
func Register(name string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if name == "" {
		panic("requestlog: plugin registered without a name")
	}
	if _, dup := factories[name]; dup {
		panic("requestlog: duplicate plugin registration: " + name)
	}
	factories[name] = f
}

// Names 返回全部已注册实现名，按字典序排序，供启动错误提示使用。
func Names() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	names := make([]string, 0, len(factories))
	for name := range factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Open 按名字取实现、创建实例并调用 Init；未知名字或 Init 失败都直接返回错误，拒绝静默兜底。
func Open(ctx context.Context, name string, raw json.RawMessage) (Plugin, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown request log implementation %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	p := f()
	if err := p.Init(ctx, raw); err != nil {
		return nil, fmt.Errorf("init request log plugin %q: %w", name, err)
	}
	return p, nil
}
