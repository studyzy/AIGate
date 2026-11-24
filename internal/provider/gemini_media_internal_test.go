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
	"strings"
	"testing"
)

func TestGeminiImageURLToPart_DataURI(t *testing.T) {
	part, err := geminiImageURLToPart("data:image/png;base64,iVBORw0KGgo=")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	inline, ok := part["inlineData"].(map[string]any)
	if !ok {
		t.Fatalf("期望 inlineData，got %v", part)
	}
	if inline["mimeType"] != "image/png" || inline["data"] != "iVBORw0KGgo=" {
		t.Fatalf("inlineData = %v", inline)
	}
	if _, has := part["fileData"]; has {
		t.Fatalf("data URI 不应产生 fileData")
	}
}

func TestGeminiImageURLToPart_HTTPSUsesFileData(t *testing.T) {
	part, err := geminiImageURLToPart("https://example.com/cat.png")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	file, ok := part["fileData"].(map[string]any)
	if !ok {
		t.Fatalf("期望 fileData，got %v", part)
	}
	if file["fileUri"] != "https://example.com/cat.png" {
		t.Fatalf("fileUri = %v", file["fileUri"])
	}
	if file["mimeType"] != "image/png" {
		t.Fatalf("mimeType = %v, want image/png", file["mimeType"])
	}
}

// http 与非 http(s) scheme 一律拒绝：http 明文且常被用于内网探测，gs:// 只有 Vertex 认。
func TestGeminiImageURLToPart_RejectsOtherSchemes(t *testing.T) {
	for _, rawURL := range []string{
		"http://example.com/cat.png",
		"gs://bucket/cat.png",
		"file:///etc/passwd",
		"ftp://example.com/cat.png",
	} {
		if _, err := geminiImageURLToPart(rawURL); err == nil {
			t.Fatalf("%q 应被拒绝", rawURL)
		}
	}
}

// 扩展名缺失的 https URL 无法推出 MIME，直接拒绝而不是让上游猜（design §5.1）。
func TestGeminiImageURLToPart_RejectsExtensionlessHTTPS(t *testing.T) {
	_, err := geminiImageURLToPart("https://example.com/picture")
	if err == nil {
		t.Fatalf("无扩展名的 https URL 应被拒绝")
	}
	if !strings.Contains(err.Error(), "extension") {
		t.Fatalf("错误文案应提示扩展名，got %v", err)
	}
}

func TestGeminiMIMETypeFromURL(t *testing.T) {
	cases := map[string]string{
		"https://e.com/a.JPG":          "image/jpeg",
		"https://e.com/a.jpeg":         "image/jpeg",
		"https://e.com/a.webp":         "image/webp",
		"https://e.com/a.png?sig=xyz":  "image/png", // query 不影响扩展名
		"https://e.com/a.png#frag":     "image/png", // fragment 不影响扩展名
		"https://e.com/v1.2/files/abc": "",          // 目录里的点不是扩展名
		"https://e.com/a":              "",          // 无扩展名
		"":                             "",
	}
	for rawURL, want := range cases {
		if got := geminiMIMETypeFromURL(rawURL); got != want {
			t.Fatalf("geminiMIMETypeFromURL(%q) = %q, want %q", rawURL, got, want)
		}
	}
}

// 空 data URI 的 payload 不能产生一个空 inlineData。
func TestGeminiImageURLToPart_RejectsEmptyDataURI(t *testing.T) {
	for _, rawURL := range []string{"data:image/png;base64,", "data:;base64,AAAA", "data:image/png,abc"} {
		if _, err := geminiImageURLToPart(rawURL); err == nil {
			t.Fatalf("%q 应被拒绝", rawURL)
		}
	}
}
