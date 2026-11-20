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

// Package webui 托管管理后台前端：把构建产物 webui/dist 经 go:embed 编进二进制，
// 在 /ui/ 下提供静态资源与单页应用回退。本包不读写数据库，也不依赖任何业务包。
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// fallbackPage 在 dist 尚未构建时替代首页，提示构建方式而不是返回空白页或目录列表。
const fallbackPage = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<title>AIGate WebUI</title>
<body style="font-family:system-ui,sans-serif;padding:3rem;line-height:1.6">
<h1>AIGate WebUI is not built</h1>
<p>Run <code>make build-webui</code> (or <code>pnpm --dir webui build</code>) and rebuild the binary.</p>
</body>
</html>
`

// Handler 返回挂在 /ui/ 下的静态资源处理器：命中文件即返回，无扩展名的未知路径回退到
// index.html 交给前端路由，其余未知资源保持 404。
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		panic("webui: embedded dist is missing: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		name := serverPath(r.URL.Path)
		target := name
		if name != "/" {
			if _, err := fs.Stat(sub, strings.TrimPrefix(name, "/")); err != nil && path.Ext(name) == "" {
				target = "/"
			}
		}
		if target == "/" {
			if _, err := fs.Stat(sub, "index.html"); err != nil {
				writeFallback(w)
				return
			}
		}

		r = r.Clone(r.Context())
		r.URL.Path = target
		w.Header().Set("Cache-Control", cacheControl(target))
		files.ServeHTTP(w, r)
	})
}

// setSecurityHeaders 给静态托管的所有响应加最小安全头。
//
// 本包是该进程里唯一输出 HTML 的地方：禁止 MIME 嗅探、禁止被第三方页面内嵌，
// 并限制页面只能加载同源资源（CSP 允许内联样式，recharts 与占位页依赖 style 属性）。
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
}

// serverPath 把 /ui 前缀去掉并清理 .. 等路径穿越，返回以 / 开头的 dist 内路径。
func serverPath(p string) string {
	p = strings.TrimPrefix(p, "/ui")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// cacheControl 让带内容哈希的静态资源长期缓存，入口页始终回源校验。
func cacheControl(name string) string {
	if strings.HasPrefix(name, "/assets/") {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func writeFallback(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, fallbackPage)
}
