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

package main

import (
	"fmt"

	"aigate/internal/version"
)

// printVersion 输出版本、commit 与构建信息（`-version` 的唯一输出，随后退出）。
func printVersion() {
	fmt.Println(version.Get().String())
}
