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

package proxy

import (
	"testing"
)

// 本文件用白盒用例钉住「落库快照解析」的纯函数契约。
//
// decodeResponsesItems 在 HTTP 层几乎不可观测：items 列恒为 NOT NULL 且由
// model.CreateResponseState 兜底，只有人为改库才能塞进 "null"。而它的返回值同时承担
// 「解码成功」与「有历史」两种语义——一旦混同，调用方会把 nil 历史当成有效快照。
// 这种函数的值就是靠直接驱动的用例守住，不看调用点覆盖率。

// TestDecodeResponsesItems_NullIsNoHistory：JSON null 解码成功但语义上「没有历史」，
// 必须与解码失败同口径返回 false，否则调用方无法区分两者。
func TestDecodeResponsesItems_NullIsNoHistory(t *testing.T) {
	for _, raw := range []string{"null", " null ", "\nnull"} {
		items, ok := decodeResponsesItems([]byte(raw))
		if ok || items != nil {
			t.Fatalf("decodeResponsesItems(%q) = (%v, %v), want (nil, false)", raw, items, ok)
		}
	}
}

// TestDecodeResponsesItems_DecodedArrayIsHistory：正常快照返回 (items, true)。
func TestDecodeResponsesItems_DecodedArrayIsHistory(t *testing.T) {
	items, ok := decodeResponsesItems([]byte(`[{"role":"user","content":"hi"}]`))
	if !ok || len(items) != 1 {
		t.Fatalf("decodeResponsesItems = (%v, %v), want one item and true", items, ok)
	}
}

// TestDecodeResponsesItems_DamagedOrEmptyIsNoHistory：损坏/空值一律降级为「无历史」。
func TestDecodeResponsesItems_DamagedOrEmptyIsNoHistory(t *testing.T) {
	for _, raw := range []string{"", "   ", "{not json", `{"role":"user"}`} {
		if items, ok := decodeResponsesItems([]byte(raw)); ok || items != nil {
			t.Fatalf("decodeResponsesItems(%q) = (%v, %v), want (nil, false)", raw, items, ok)
		}
	}
}
