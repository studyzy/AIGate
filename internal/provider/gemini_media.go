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

// 本文件把 OpenAI 的 image_url 值转换成 Gemini 的 part。
//
// 两条路径，都不产生网关侧出站请求（design §5.1）：
//   - base64 data URI → inlineData {mimeType, data}
//   - https URL       → fileData  {fileUri, mimeType}，由 Google 去抓取
//
// 显式拒绝「网关代下载」：那会让终端用户通过 image_url 指定任意出站目标。

import (
	"errors"
	"fmt"
	"strings"
)

// geminiImageMIMETypes 是显式扩展名表，不用 mime.TypeByExtension
// （后者读宿主 /etc/mime.types 并附加 charset，使线上行为依赖运行环境）。
// 只收图片：非图片媒体不在本协议支持范围内（design §2 非目标）。
var geminiImageMIMETypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
	".heic": "image/heic",
	".heif": "image/heif",
}

// geminiImageURLToPart 把一个 image_url 值转成 Gemini part，失败时返回英文错误。
func geminiImageURLToPart(rawURL string) (map[string]any, error) {
	if strings.HasPrefix(rawURL, "data:") {
		mimeType, data := parseDataURI(rawURL)
		if mimeType == "" || data == "" {
			return nil, errors.New("invalid base64 image data URI")
		}
		return map[string]any{"inlineData": map[string]any{
			"mimeType": mimeType,
			"data":     data,
		}}, nil
	}

	if !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("image URL scheme not supported, use https or a base64 data URI: %s", rawURL)
	}

	mimeType := geminiMIMETypeFromURL(rawURL)
	if mimeType == "" {
		return nil, errors.New("image URL must carry a file extension (.png/.jpg/...) or be a base64 data URI")
	}
	return map[string]any{"fileData": map[string]any{
		"mimeType": mimeType,
		"fileUri":  rawURL,
	}}, nil
}

// geminiMIMETypeFromURL 按 URL 最后一段的扩展名查表；查不到返回空串。
func geminiMIMETypeFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	// 先剥 query 与 fragment（"a.png?sig=1" 仍要能命中），
	// 再只看最后一段路径（避免 "/v1.2/files/abc" 被当成扩展名 ".2/files/abc"）。
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	if i := strings.LastIndex(rawURL, "/"); i >= 0 {
		rawURL = rawURL[i+1:]
	}
	dot := strings.LastIndex(rawURL, ".")
	if dot < 0 {
		return ""
	}
	return geminiImageMIMETypes[strings.ToLower(rawURL[dot:])]
}
