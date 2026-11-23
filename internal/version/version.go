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

// Package version 汇总网关的构建期信息（版本号、commit、构建时间）与进程启动时间，
// 供 `-version`、`GET /version` 与 aigatectl 共用，避免多处硬编码。
//
// 版本号由链接器注入（`-ldflags "-X aigate/internal/version.Version=..."`）；
// commit 与构建时间优先取注入值，缺失时回退到 Go 工具链自动写入的 VCS 元数据
// （debug.ReadBuildInfo），因此裸 `go build` 也能得到真实 commit。
package version

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

// Version 是网关语义版本号，默认值即未注入时的版本；发布构建由链接器覆盖，
// 能力变更时仍需与 CHANGELOG.md 同步递增。
var Version = "0.3.0"

// Commit 与 BuildTime 是链接器注入的 commit 与构建时间；为空时回退到 VCS 元数据。
var (
	Commit    string
	BuildTime string
)

// Info 描述一次运行的构建与启动信息。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
	StartedAt string `json:"started_at"`
}

var (
	startOnce sync.Once
	startedAt time.Time
)

// Start 记录进程启动时间；重复调用只保留首次，由 main 在初始化早期调用一次。
func Start() {
	startOnce.Do(func() {
		startedAt = time.Now()
	})
}

// Get 返回当前运行的构建信息；未注入的字段以 "unknown" 填充，从不返回空串。
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info.GoVersion = bi.GoVersion
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.BuildTime == "" {
					info.BuildTime = s.Value
				}
			case "vcs.modified":
				info.Dirty = s.Value == "true"
			}
		}
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.BuildTime == "" {
		info.BuildTime = "unknown"
	}
	if startedAt.IsZero() {
		info.StartedAt = "unknown"
	} else {
		info.StartedAt = startedAt.UTC().Format(time.RFC3339)
	}
	return info
}

// String 渲染 `-version` 的多行输出。
func (i Info) String() string {
	return fmt.Sprintf(
		"Tencent Cloud AIGate %s\ncommit: %s\ndirty: %t\nbuild time: %s\ngo version: %s",
		i.Version, i.Commit, i.Dirty, i.BuildTime, i.GoVersion,
	)
}
