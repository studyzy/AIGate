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

// 本文件实现自包含的 sqlite 请求日志插件：进程内独占一个 SQLite 文件，表结构、索引、
// PRAGMA 与配置校验全部自带，不复用也不改动核心库的 schema（sql/init.sql、internal/model）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const (
	// defaultSQLitePath 是未配置 path 时使用的默认文件（当前工作目录下）。
	defaultSQLitePath = "request-logs.db"
	// defaultSQLiteRetentionDays 是未配置 retention_days 时的默认保留天数。
	defaultSQLiteRetentionDays = 7
	// maxSQLiteRetentionDays 与 internal/config 的 maxRetentionDays（36500，即 100 年）一致：
	// 挡住 time.Duration(days)*24*time.Hour 溢出为负数而被当成「永不清理」。
	// 此处按同一上界硬编码，不 import internal/config，避免插件与核心配置耦合。
	maxSQLiteRetentionDays = 36500
	// defaultSQLitePageSize 与 queryparams.DefaultPageSize 一致，供零值 Query 兜底。
	defaultSQLitePageSize = 50
	// sqliteListColumns 是列表查询显式选取的列：排除四个正文/头列，避免列表回带大字段。
	sqliteListColumns = "id, created_at, method, path, model, api_key_hash, status_code, latency_ms, stream, error, truncated"
)

var (
	errSQLiteClosed         = errors.New("request log sqlite plugin is closed")
	errSQLiteNotInitialized = errors.New("request log sqlite plugin is not initialized")
)

// sqliteConfig 是插件私有配置；retention_days 为 0 表示永不清理。
type sqliteConfig struct {
	Path          string `json:"path"`
	RetentionDays int    `json:"retention_days"`
}

// sqliteRow 是插件自有表 request_logs 的 GORM 模型，列与 Entry 一一对应。
type sqliteRow struct {
	ID              string    `gorm:"column:id;primaryKey"`
	CreatedAt       time.Time `gorm:"column:created_at;index:idx_rl_created_at;index:idx_rl_created_hash,priority:1"`
	Method          string    `gorm:"column:method"`
	Path            string    `gorm:"column:path;index:idx_rl_path"`
	Model           string    `gorm:"column:model;index:idx_rl_model"`
	APIKeyHash      string    `gorm:"column:api_key_hash;index:idx_rl_api_key_hash;index:idx_rl_created_hash,priority:2"`
	StatusCode      int       `gorm:"column:status_code;index:idx_rl_status_code"`
	LatencyMS       int       `gorm:"column:latency_ms;index:idx_rl_latency_ms"`
	Stream          bool      `gorm:"column:stream"`
	RequestBody     string    `gorm:"column:request_body"`
	ResponseBody    string    `gorm:"column:response_body"`
	RequestHeaders  string    `gorm:"column:request_headers"`
	ResponseHeaders string    `gorm:"column:response_headers"`
	Error           string    `gorm:"column:error"`
	Truncated       bool      `gorm:"column:truncated"`
}

// TableName 固定表名；不声明会被 GORM 按 Go 类型名复数化成 sqlite_rows（R17）。
func (sqliteRow) TableName() string { return "request_logs" }

// sqlitePlugin 独占一个 SQLite 文件；mu 保护关闭与在途读写的竞态（R18）。
type sqlitePlugin struct {
	mu sync.RWMutex

	db        *gorm.DB
	sqlDB     *sql.DB
	retention time.Duration
	closed    bool
}

var (
	_ Plugin = (*sqlitePlugin)(nil)
	_ Reader = (*sqlitePlugin)(nil)
)

func init() {
	Register("sqlite", func() Plugin { return &sqlitePlugin{} })
}

func (*sqlitePlugin) Name() string { return "sqlite" }

// parseSQLiteConfig 解析并校验私有配置：先把默认值写进结构体，再用 raw 覆盖，
// 因此 {} 用默认值、显式 0 才是「永不清理」；非法值一律拒绝，不做静默归一化。
// 解码用 DisallowUnknownFields，拼错的未知键（如 retention_day）在启动期直接报错，
// 而不是被忽略后悄悄回退默认保留期（T1）。
func parseSQLiteConfig(raw json.RawMessage) (sqliteConfig, error) {
	cfg := sqliteConfig{
		Path:          defaultSQLitePath,
		RetentionDays: defaultSQLiteRetentionDays,
	}
	if len(raw) > 0 {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return sqliteConfig{}, errors.New("sqlite config must be a JSON object")
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return sqliteConfig{}, fmt.Errorf("invalid sqlite config: %w", err)
		}
	}
	if cfg.Path == "" {
		return sqliteConfig{}, errors.New("sqlite path must not be empty")
	}
	if cfg.RetentionDays < 0 || cfg.RetentionDays > maxSQLiteRetentionDays {
		return sqliteConfig{}, fmt.Errorf("sqlite retention_days must be between 0 and %d", maxSQLiteRetentionDays)
	}
	return cfg, nil
}

// sqlitePragmaDSN 把并发 PRAGMA 注入 DSN：busy_timeout 与 journal_mode 对每个连接生效，
// 与 internal/model 的同名做法一致；调用方已显式带 _pragma 时保持原样。
func sqlitePragmaDSN(path string) string {
	if strings.Contains(path, "_pragma=") {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

// Init 打开自有库、设置 WAL、建表建索引。失败时关闭已打开的连接，不留句柄。
func (p *sqlitePlugin) Init(ctx context.Context, raw json.RawMessage) error {
	cfg, err := parseSQLiteConfig(raw)
	if err != nil {
		return err
	}
	db, err := gorm.Open(sqlite.Open(sqlitePragmaDSN(cfg.Path)), &gorm.Config{
		// 静默日志：正文可能含敏感数据，且避免 record-not-found 噪声进入日志。
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return fmt.Errorf("open request log sqlite %q: %w", cfg.Path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get request log sqlite db: %w", err)
	}
	// WAL 是库级持久化设置，显式执行一次确保落地；busy_timeout 由 DSN 逐连接生效。
	if err := db.WithContext(ctx).Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("set journal_mode=WAL: %w", err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&sqliteRow{}); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("migrate request_logs: %w", err)
	}

	p.mu.Lock()
	p.db = db
	p.sqlDB = sqlDB
	p.retention = time.Duration(cfg.RetentionDays) * 24 * time.Hour
	p.closed = false
	p.mu.Unlock()
	return nil
}

// acquire 在关闭后返回良性错误；R18：Stop 超时后 Close 与在途 Write/Prune 并发时不 panic。
func (p *sqlitePlugin) acquire() (*gorm.DB, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, errSQLiteClosed
	}
	if p.db == nil {
		return nil, errSQLiteNotInitialized
	}
	return p.db, nil
}

// fromEntry 把契约 Entry 映射为自有表行；创建时间统一转 UTC。
func fromEntry(e Entry) sqliteRow {
	created := e.CreatedAt.UTC()
	if created.IsZero() {
		// 零值若交给 GORM 自动填充会用本地时区时间，破坏 created_at 的字典序比较。
		created = time.Now().UTC()
	}
	return sqliteRow{
		ID:              e.ID,
		CreatedAt:       created,
		Method:          e.Method,
		Path:            e.Path,
		Model:           e.Model,
		APIKeyHash:      e.APIKeyHash,
		StatusCode:      e.StatusCode,
		LatencyMS:       e.LatencyMS,
		Stream:          e.Stream,
		RequestBody:     e.RequestBody,
		ResponseBody:    e.ResponseBody,
		RequestHeaders:  e.RequestHeaders,
		ResponseHeaders: e.ResponseHeaders,
		Error:           e.Error,
		Truncated:       e.Truncated,
	}
}

func toRow(r sqliteRow) Row {
	return Row{
		ID:         r.ID,
		CreatedAt:  r.CreatedAt,
		Method:     r.Method,
		Path:       r.Path,
		Model:      r.Model,
		APIKeyHash: r.APIKeyHash,
		StatusCode: r.StatusCode,
		LatencyMS:  r.LatencyMS,
		Stream:     r.Stream,
		Error:      r.Error,
		Truncated:  r.Truncated,
	}
}

func toEntry(r sqliteRow) Entry {
	return Entry{
		ID:              r.ID,
		CreatedAt:       r.CreatedAt,
		Method:          r.Method,
		Path:            r.Path,
		Model:           r.Model,
		APIKeyHash:      r.APIKeyHash,
		StatusCode:      r.StatusCode,
		LatencyMS:       r.LatencyMS,
		Stream:          r.Stream,
		RequestBody:     r.RequestBody,
		ResponseBody:    r.ResponseBody,
		RequestHeaders:  r.RequestHeaders,
		ResponseHeaders: r.ResponseHeaders,
		Error:           r.Error,
		Truncated:       r.Truncated,
	}
}

// escapeLike 转义 LIKE 的 %/_/\，保证 Q 按字面子串匹配（Review Focus 4）。
func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// buildSQLiteWhere 逐项拼装 Query 的过滤条件；Start/End 是已解析好的 UTC 时刻，
// 用半开区间 [Start, End)，绝不使用 BETWEEN（R16）。
func buildSQLiteWhere(q Query) (string, []any) {
	var conds []string
	var args []any
	if q.Start != nil {
		conds = append(conds, "created_at >= ?")
		args = append(args, q.Start.UTC())
	}
	if q.End != nil {
		conds = append(conds, "created_at < ?")
		args = append(args, q.End.UTC())
	}
	if q.APIKeyHash != "" {
		conds = append(conds, "api_key_hash = ?")
		args = append(args, q.APIKeyHash)
	}
	if q.Model != "" {
		conds = append(conds, "model = ?")
		args = append(args, q.Model)
	}
	if q.Path != "" {
		conds = append(conds, "path = ?")
		args = append(args, q.Path)
	}
	if q.StatusCode != 0 {
		conds = append(conds, "status_code = ?")
		args = append(args, q.StatusCode)
	}
	if q.Stream != nil {
		conds = append(conds, "stream = ?")
		args = append(args, *q.Stream)
	}
	if q.MinLatencyMS > 0 {
		conds = append(conds, "latency_ms >= ?")
		args = append(args, q.MinLatencyMS)
	}
	if q.MaxLatencyMS > 0 {
		conds = append(conds, "latency_ms <= ?")
		args = append(args, q.MaxLatencyMS)
	}
	if q.Q != "" {
		conds = append(conds, "(request_body LIKE ? ESCAPE '\\' OR response_body LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(q.Q) + "%"
		args = append(args, like, like)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return strings.Join(conds, " AND "), args
}

// Write 批量插入，按主键 id 幂等（重复采集/重放不产生重复行）。
func (p *sqlitePlugin) Write(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	db, err := p.acquire()
	if err != nil {
		return err
	}
	rows := make([]sqliteRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, fromEntry(e))
	}
	if err := db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		CreateInBatches(rows, BatchSize).Error; err != nil {
		return fmt.Errorf("write request logs: %w", err)
	}
	return nil
}

// Search 先 Count 再分页取列表；列表用 Select 排除四个正文/头列，Rows 恒为非 nil 切片。
func (p *sqlitePlugin) Search(ctx context.Context, q Query) (Page, error) {
	empty := Page{Rows: []Row{}}
	db, err := p.acquire()
	if err != nil {
		return empty, err
	}
	page, pageSize := q.Page, q.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultSQLitePageSize
	}
	where, args := buildSQLiteWhere(q)

	// 每次新建链，避免 Count 与 Find 共用 *gorm.DB 时互相污染 statement。
	scope := func() *gorm.DB {
		tx := db.WithContext(ctx).Model(&sqliteRow{})
		if where != "" {
			tx = tx.Where(where, args...)
		}
		return tx
	}

	var total int64
	if err := scope().Count(&total).Error; err != nil {
		return empty, fmt.Errorf("count request logs: %w", err)
	}
	var rows []sqliteRow
	if err := scope().
		Select(sqliteListColumns).
		Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error; err != nil {
		return empty, fmt.Errorf("list request logs: %w", err)
	}
	out := Page{
		Rows:     make([]Row, 0, len(rows)),
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, toRow(r))
	}
	return out, nil
}

// Get 返回含正文的完整 Entry；查不到映射为契约哨兵 ErrNotFound。
func (p *sqlitePlugin) Get(ctx context.Context, id string) (*Entry, error) {
	db, err := p.acquire()
	if err != nil {
		return nil, err
	}
	var row sqliteRow
	if err := db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get request log %q: %w", id, err)
	}
	e := toEntry(row)
	return &e, nil
}

// Prune 删除创建时间早于 before 的行（半开），返回实际删除条数。
func (p *sqlitePlugin) Prune(ctx context.Context, before time.Time) (int64, error) {
	db, err := p.acquire()
	if err != nil {
		return 0, err
	}
	tx := db.WithContext(ctx).Where("created_at < ?", before.UTC()).Delete(&sqliteRow{})
	if tx.Error != nil {
		return 0, fmt.Errorf("prune request logs: %w", tx.Error)
	}
	return tx.RowsAffected, nil
}

// Retention 返回配置的保留期；0 表示核心不启动清理循环。
func (p *sqlitePlugin) Retention() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.retention
}

// Close 关闭底层 sql.DB；幂等且并发安全（R18），Init 失败后调用也安全。
func (p *sqlitePlugin) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sqlDB := p.sqlDB
	p.mu.Unlock()

	if sqlDB == nil {
		return nil
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("close request log sqlite db: %w", err)
	}
	return nil
}
