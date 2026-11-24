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

package provider

// 本文件实现 Gemini thoughtSignature 在 tool-call id 上的编解码。
// 方案（design §5.2，对齐 Bifrost）：
//   - 回给客户端 <baseID>_ts_<base64url 签名>
//   - 回传时拆出签名放进 Part.ThoughtSignature
//   - 发上游前必须剥回 baseID

import (
	"encoding/base64"
	"strings"
)

const (
	// geminiSignatureSeparator 分隔 base id 与其中的签名。
	geminiSignatureSeparator = "_ts_"
	// geminiSkipSignatureValidator 是 Gemini 认可的哨兵：收到它即跳过签名校验，
	// 用于 id 里找不到签名时的降级，而不是报 400。
	geminiSkipSignatureValidator = "skip_thought_signature_validator"
)

// encodeGeminiCallID 把签名编码进 call id；sig 为空时原样返回 baseID。
// 必须用 base64.RawURLEncoding（含 - 与 _，不含 + / =）。
func encodeGeminiCallID(baseID string, sig []byte) string {
	if len(sig) == 0 {
		return baseID
	}
	return baseID + geminiSignatureSeparator + base64.RawURLEncoding.EncodeToString(sig)
}

// splitGeminiCallID 拆出 base id 与签名；无分隔符或签名解码失败时签名为 nil。
func splitGeminiCallID(callID string) (string, []byte) {
	base, tail, found := strings.Cut(callID, geminiSignatureSeparator)
	if !found {
		return callID, nil
	}
	sig, err := base64.RawURLEncoding.DecodeString(tail)
	if err != nil {
		return base, nil
	}
	return base, sig
}

// stripGeminiCallID 恒返回不含签名的 base id。
func stripGeminiCallID(callID string) string {
	base, _ := splitGeminiCallID(callID)
	return base
}
