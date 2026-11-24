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

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncodeSplitGeminiCallID_RoundTrip(t *testing.T) {
	sig := []byte{0x01, 0x02, 0x03, 0xff}
	encoded := encodeGeminiCallID("call_abc", sig)
	if encoded == "call_abc" {
		t.Fatalf("带签名时 id 必须变化")
	}
	base, got := splitGeminiCallID(encoded)
	if base != "call_abc" {
		t.Fatalf("base = %q, want call_abc", base)
	}
	if !bytes.Equal(got, sig) {
		t.Fatalf("sig = %x, want %x", got, sig)
	}
	if got := stripGeminiCallID(encoded); got != "call_abc" {
		t.Fatalf("strip = %q, want call_abc", got)
	}
}

func TestEncodeGeminiCallID_EmptySignatureUnchanged(t *testing.T) {
	if got := encodeGeminiCallID("call_abc", nil); got != "call_abc" {
		t.Fatalf("空签名不应改动 id，got %q", got)
	}
	if got := encodeGeminiCallID("call_abc", []byte{}); got != "call_abc" {
		t.Fatalf("空切片不应改动 id，got %q", got)
	}
}

func TestSplitGeminiCallID_NoSeparator(t *testing.T) {
	base, sig := splitGeminiCallID("call_abc")
	if base != "call_abc" || sig != nil {
		t.Fatalf("base=%q sig=%v，无分隔符时应原样返回", base, sig)
	}
}

// 签名部分不是合法 base64url 时不应把 id 整个吞掉：仍返回 base id、签名为 nil。
func TestSplitGeminiCallID_MalformedSignature(t *testing.T) {
	base, sig := splitGeminiCallID("call_abc_ts_!!!not-base64!!!")
	if base != "call_abc" {
		t.Fatalf("base = %q, want call_abc", base)
	}
	if sig != nil {
		t.Fatalf("sig = %x, want nil", sig)
	}
}

// 签名使用 base64url（含 - 与 _），不能用标准 base64。
func TestEncodeGeminiCallID_UsesRawURLEncoding(t *testing.T) {
	sig := []byte{0xfb, 0xff} // 标准 base64 会出 '+' 和 '/'
	encoded := encodeGeminiCallID("c", sig)
	if strings.ContainsAny(encoded, "+/=") {
		t.Fatalf("id 含标准 base64 字符: %q", encoded)
	}
	if _, got := splitGeminiCallID(encoded); !bytes.Equal(got, sig) {
		t.Fatalf("sig round-trip = %x", got)
	}
}

// 上游用标准 base64 回签名，客户端回传时必须能无损还原成同一组字节，
// 否则签名校验失败。覆盖含 '+'、'/'、'=' 的字节组合。
func TestGeminiSignatureSurvivesStdBase64RoundTrip(t *testing.T) {
	for _, sig := range [][]byte{
		{0xfb, 0xff},       // 标准 base64 含 '+'
		{0xff, 0xfe},       // 含 '/'
		{0x01, 0x02, 0x03}, // 含 '='
		{0xfb, 0xff, 0xbf}, // 多字节组合
	} {
		upstream := base64.StdEncoding.EncodeToString(sig)
		raw := geminiDecodeSignature(upstream)
		if !bytes.Equal(raw, sig) {
			t.Fatalf("decode(%q) = %x, want %x", upstream, raw, sig)
		}
		_, back := splitGeminiCallID(encodeGeminiCallID("call_x", raw))
		if !bytes.Equal(back, sig) {
			t.Fatalf("sig %x: round-trip = %x", sig, back)
		}
	}
}
