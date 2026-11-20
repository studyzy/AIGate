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

package client

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ErrMissingToken 表示请求面需要令牌，但配置中对应的令牌为空。
var ErrMissingToken = errors.New("missing token")

// ExitCode 把 HTTP 状态码映射到 CLI 退出码：2xx→0、401/403→3、其它→4。
func ExitCode(status int) int {
	switch {
	case status >= 200 && status < 300:
		return 0
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return 3
	default:
		return 4
	}
}

// ErrorMessage 从统一错误体 {"error":{"message",...}} 中取出 message；
// 解析失败或不是该形状时原样返回响应体文本。
func ErrorMessage(body []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Message == "" {
		return string(body)
	}
	return env.Error.Message
}
