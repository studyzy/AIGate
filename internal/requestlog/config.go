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

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// LoadRawConfig 装载请求日志插件的原始配置（与注册表/插件契约相互独立，不依赖其类型）：
//   - file 非空时优先读取文件内容（忽略 inline）；
//   - inline 与 file 都为空时返回空对象 "{}"；
//   - 内联配置非空但不是合法 JSON 对象（首个非空字符不是 '{'）时返回错误；
//   - 文件不存在/读失败返回包装后的底层错误。
//
// 返回值恒为合法 JSON 对象，可直接交给 Open 作为 Init 的 raw 参数。
func LoadRawConfig(inline, file string) (json.RawMessage, error) {
	var data []byte
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read request log config file %q: %w", file, err)
		}
		data = b
	case inline != "":
		data = []byte(inline)
	default:
		return json.RawMessage("{}"), nil
	}
	if !isJSONObject(data) {
		return nil, errors.New("invalid request log config")
	}
	return json.RawMessage(data), nil
}

// isJSONObject 判断 b 是否为合法 JSON 对象：首个非空字符为 '{' 且整体为合法 JSON。
func isJSONObject(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(b)
}
