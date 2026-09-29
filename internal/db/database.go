// Package db 封装 GORM 数据库初始化、DSN 处理与写入重试。
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"llama_proxy/internal/config"

	"github.com/glebarez/sqlite"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/youcd/toolkit/log"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"gorm.io/gorm/logger"
)

// Database 是 GORM 数据库句柄，全项目统一使用。
type Database = *gorm.DB

// CloseDatabase 关闭底层连接池（gorm.DB 自身没有 Close 方法）。
func CloseDatabase(db Database) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// IsPostgresDB 判断 GORM 句柄底层是否为 PostgreSQL。
func IsPostgresDB(db Database) bool {
	return db.Dialector.Name() == "postgres"
}

func NewDatabase(cfg config.DatabaseConfig, dataDir, logLevel string) (Database, error) {
	switch strings.ToLower(cfg.Type) {
	case "postgresql", "postgres", "pg":
		return newPostgreSQLDatabase(cfg.PostgreSQL)
	case "sqlite", "":
		return newSQLiteDatabase(cfg.SQLite, dataDir, logLevel)
	default:
		return nil, fmt.Errorf("unsupported database type: %s", cfg.Type)
	}
}

func newSQLiteDatabase(cfg config.SQLiteConfig, dataDir, logLevel string) (Database, error) {
	dbPath := cfg.Path
	if !strings.HasPrefix(dbPath, "/") {
		dbPath = dataDir + "/" + dbPath
	}

	dsn := "file:" + dbPath +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)"

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: log.NewGormLogger(time.Second, logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(0)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	return db, nil
}

func newPostgreSQLDatabase(cfg config.PostgreSQLConfig) (Database, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("postgresql DSN is required")
	}

	if err := ensurePostgresDatabase(cfg.DSN); err != nil {
		return nil, err
	}

	db, err := gorm.Open(gormpostgres.Open(cfg.DSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgresql: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open postgresql: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifetime) * time.Second)

	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgresql: %w", err)
	}

	return db, nil
}

// ensurePostgresDatabase connects to the target database and, if it does not
// exist, creates it automatically by connecting to the maintenance database
// ("postgres") first.
func ensurePostgresDatabase(dsn string) error {
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open postgresql: %w", err)
	}
	probeErr := probe.Ping()
	_ = probe.Close()
	if probeErr == nil {
		return nil
	}
	if !isMissingDatabaseError(probeErr) {
		return fmt.Errorf("connect postgresql: %w", probeErr)
	}

	dbname, err := pgDatabaseName(dsn)
	if err != nil {
		return fmt.Errorf("parse postgresql dsn: %w", err)
	}
	if dbname == "" {
		return fmt.Errorf("cannot determine database name from DSN (missing dbname)")
	}

	adminDSN, err := pgAdminDSN(dsn)
	if err != nil {
		return fmt.Errorf("build admin postgresql dsn: %w", err)
	}
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return fmt.Errorf("open postgresql admin connection: %w", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		return fmt.Errorf("cannot connect to postgres maintenance database for auto-create: %w", err)
	}

	quoted := `"` + strings.ReplaceAll(dbname, `"`, `""`) + `"`
	if _, err := admin.Exec(`CREATE DATABASE ` + quoted); err != nil {
		return fmt.Errorf("auto-create database %s: %w", quoted, err)
	}
	return nil
}

// pgDatabaseName extracts the dbname/database parameter from a lib/pq DSN
// (URL or key=value form).
func pgDatabaseName(dsn string) (string, error) {
	kv, err := parsePostgresKV(dsn)
	if err != nil {
		return "", err
	}
	if kv["dbname"] != "" {
		return kv["dbname"], nil
	}
	return kv["database"], nil
}

// pgAdminDSN returns a DSN pointed at the "postgres" maintenance database,
// preserving the user, host and options from the original DSN.
func pgAdminDSN(dsn string) (string, error) {
	kv, err := parsePostgresKV(dsn)
	if err != nil {
		return "", err
	}
	delete(kv, "dbname")
	delete(kv, "database")
	kv["dbname"] = "postgres"

	parts := make([]string, 0, len(kv))
	for k, v := range kv {
		if strings.ContainsAny(v, " '\\") {
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " "), nil
}

// postgresURLToKV converts a URL-form DSN (postgres://user:pass@host:port/db?opts)
// into a key=value DSN, mimicking lib/pq's ParseURL.
func postgresURLToKV(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	kv := make(map[string]string)
	if u.Host != "" {
		if u.Port() != "" {
			kv["host"] = u.Hostname()
			kv["port"] = u.Port()
		} else {
			kv["host"] = u.Host
		}
	}
	if u.User != nil {
		kv["user"] = u.User.Username()
		if pass, ok := u.User.Password(); ok {
			kv["password"] = pass
		}
	}
	if u.Path != "" && u.Path != "/" {
		kv["dbname"] = strings.TrimPrefix(u.Path, "/")
	}
	for k, vs := range u.Query() {
		if len(vs) > 0 {
			kv[k] = vs[0]
		}
	}
	parts := make([]string, 0, len(kv))
	for k, v := range kv {
		if strings.ContainsAny(v, " '\\") {
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " "), nil
}

// parsePostgresKV parses a postgres DSN into a key/value map. Both URL form
// (postgres://...) and key=value form are accepted.
func parsePostgresKV(dsn string) (map[string]string, error) {
	if strings.Contains(dsn, "://") {
		parsed, err := postgresURLToKV(dsn)
		if err != nil {
			return nil, err
		}
		dsn = parsed
	}

	kv := make(map[string]string)
	for _, field := range splitDSNFields(dsn) {
		idx := strings.IndexByte(field, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid dsn field %q", field)
		}
		key := strings.TrimSpace(field[:idx])
		val := strings.TrimSpace(field[idx+1:])
		if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			val = val[1 : len(val)-1]
			val = strings.ReplaceAll(val, "''", "'")
		}
		kv[key] = val
	}
	return kv, nil
}

// splitDSNFields splits a key=value DSN on spaces, honouring single-quoted values.
func splitDSNFields(dsn string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range dsn {
		switch {
		case r == '\'':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}

// isMissingDatabaseError reports whether err indicates the target PostgreSQL
// database does not exist (SQLSTATE 3D000 / invalid_catalog_name).
func isMissingDatabaseError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "3D000" || pgErr.Code == "42P01"
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "does not exist.")
}

var sqliteDDLStatements = []string{
	`CREATE TABLE IF NOT EXISTS requests (
		id TEXT PRIMARY KEY,
		created_at DATETIME NOT NULL,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		query TEXT,
		client_ip TEXT,
		backend_url TEXT,
		model TEXT,
		is_streaming INTEGER NOT NULL DEFAULT 0,
		status_code INTEGER NOT NULL DEFAULT 0,
		error_text TEXT,
		request_bytes INTEGER NOT NULL DEFAULT 0,
		response_bytes INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		cached_prompt_tokens INTEGER NOT NULL DEFAULT 0,
		cache_hit_pct REAL NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		prompt_ms REAL NOT NULL DEFAULT 0,
		completion_ms REAL NOT NULL DEFAULT 0,
		total_ms REAL NOT NULL DEFAULT 0,
		first_byte_ms REAL NOT NULL DEFAULT 0,
		chunks_count INTEGER NOT NULL DEFAULT 0,
		request_raw_path TEXT,
		response_raw_path TEXT,
		user_agent TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_path ON requests(path);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_status_code ON requests(status_code);`,
	`CREATE TABLE IF NOT EXISTS backend_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at DATETIME NOT NULL,
		backend_url TEXT NOT NULL,
		metric_name TEXT NOT NULL,
		metric_value REAL NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_backend_metrics_created_at ON backend_metrics(created_at DESC);`,
}

var pgDDLStatements = []string{
	`CREATE TABLE IF NOT EXISTS requests (
		id TEXT PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		query TEXT,
		client_ip TEXT,
		backend_url TEXT,
		model TEXT,
		is_streaming BOOLEAN NOT NULL DEFAULT FALSE,
		status_code INTEGER NOT NULL DEFAULT 0,
		error_text TEXT,
		request_bytes BIGINT NOT NULL DEFAULT 0,
		response_bytes BIGINT NOT NULL DEFAULT 0,
		prompt_tokens BIGINT NOT NULL DEFAULT 0,
		cached_prompt_tokens BIGINT NOT NULL DEFAULT 0,
		cache_hit_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		prompt_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
		completion_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
		total_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
		first_byte_ms DOUBLE PRECISION NOT NULL DEFAULT 0,
		chunks_count BIGINT NOT NULL DEFAULT 0,
		request_raw_path TEXT,
		response_raw_path TEXT,
		user_agent TEXT
	);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_created_at ON requests(created_at DESC);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_path ON requests(path);`,
	`CREATE INDEX IF NOT EXISTS idx_requests_status_code ON requests(status_code);`,
	`CREATE TABLE IF NOT EXISTS backend_metrics (
		id SERIAL PRIMARY KEY,
		created_at TIMESTAMPTZ NOT NULL,
		backend_url TEXT NOT NULL,
		metric_name TEXT NOT NULL,
		metric_value DOUBLE PRECISION NOT NULL
	);`,
	`CREATE INDEX IF NOT EXISTS idx_backend_metrics_created_at ON backend_metrics(created_at DESC);`,
}

func initSQLiteDB(db Database) error {
	for _, stmt := range sqliteDDLStatements {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

func initPostgreSQLDB(db Database) error {
	for _, stmt := range pgDDLStatements {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

func InitDB(db Database, dbType string) error {
	switch strings.ToLower(dbType) {
	case "postgresql", "postgres", "pg":
		return initPostgreSQLDB(db)
	default:
		return initSQLiteDB(db)
	}
}

// RetryWrite 对 SQLite/Postgres 的 busy/deadlock 类错误做有限次退避重试。
func RetryWrite(op func() error) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		if !isBusyError(err) {
			return err
		}
		time.Sleep(time.Duration(40*(attempt+1)) * time.Millisecond)
	}
	return err
}

func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pat := range []string{
		"database is locked",
		"sqlbusy",
		"sqlite_busy",
		"busy",
		"deadlock detected",
		"deadlock_detected",
		"could not serialize access",
		"serialization_failure",
		"lock timeout",
		"lock_timeout",
		"database table is locked",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}
