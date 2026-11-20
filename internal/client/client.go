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

// Package client 提供 aigatectl 使用的唯一 HTTP 客户端：按面（admin/user/none）
// 选择令牌、拼接请求、原样读回响应体，是后续 CLI 与 WebUI 的复用点。
// 本包只负责传输，不判定状态码、不解析响应体，也不产生诊断输出或退出码。
package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config 是构造 Client 所需的配置。
type Config struct {
	BaseURL    string
	AdminToken string
	UserToken  string
	Timeout    time.Duration
}

// Client 复用同一份配置与 http.Client 的轻量客户端。
type Client struct {
	baseURL    string
	adminToken string
	userToken  string
	httpClient *http.Client
}

// Plane 表示请求所属的接口面，决定使用哪一枚令牌。
type Plane int

const (
	PlaneAdmin Plane = iota
	PlaneUser
	PlaneNone
)

// Request 描述一次待发出的 HTTP 请求。
type Request struct {
	Plane  Plane
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// Result 原样保存响应状态码与完整响应体。
type Result struct {
	StatusCode int
	Body       []byte
}

// New 校验 BaseURL 并构造 Client；scheme 非 http/https 或缺 host 时返回 error。
// 缺 host 的 URL（如 http://）必须本地拒绝，否则 Do 会把它当作传输错误误判成
// 退出码 5，spec §7 要求非法 --url 一律本地报错退出 2。
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid base URL scheme %q: must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q: missing host", cfg.BaseURL)
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		adminToken: cfg.AdminToken,
		userToken:  cfg.UserToken,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// Do 按 Plane 选令牌后发出请求，并把响应体原样读回；不判定状态码。
func (c *Client) Do(ctx context.Context, r Request) (*Result, error) {
	token, err := c.tokenFor(r.Plane)
	if err != nil {
		return nil, err
	}

	fullURL := c.baseURL + r.Path
	if len(r.Query) > 0 {
		fullURL += "?" + r.Query.Encode()
	}

	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, fullURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if r.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return &Result{StatusCode: resp.StatusCode, Body: data}, nil
}

// tokenFor 按 Plane 取令牌：admin 面用 adminToken，user 面优先 userToken、
// 缺失时回退 adminToken（auth.RequireUser 允许 admin 角色），两枚都缺才返回
// ErrMissingToken；PlaneNone 及未知取值不携带令牌。
func (c *Client) tokenFor(p Plane) (string, error) {
	switch p {
	case PlaneAdmin:
		if c.adminToken == "" {
			return "", ErrMissingToken
		}
		return c.adminToken, nil
	case PlaneUser:
		if c.userToken != "" {
			return c.userToken, nil
		}
		if c.adminToken != "" {
			return c.adminToken, nil
		}
		return "", ErrMissingToken
	default:
		// PlaneNone 及未知取值都不携带令牌。
		return "", nil
	}
}
