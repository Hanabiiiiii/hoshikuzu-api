package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"random-image-api/internal/config"
	"random-image-api/internal/model"

	_ "modernc.org/sqlite"
)

func Open(cfg config.Config) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Join(cfg.Storage.DataDir, "tmp"), 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	for _, typ := range []model.ImageType{model.Desktop, model.Mobile} {
		if err := os.MkdirAll(filepath.Join(cfg.Storage.StorageDir, string(typ), "thumb"), 0755); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", typ, err)
		}
	}

	dbPath := filepath.Join(cfg.Storage.DataDir, "images.db")

	dsn := "file:" + dbPath + "?" + strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=cache_size(-30000)",
		"_pragma=mmap_size(268435456)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=foreign_keys(ON)",
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite 是单写者，读连接多一点没有收益，反而容易触发锁竞争。
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	_, _ = db.Exec(`PRAGMA optimize`)

	return db, nil
}

func migrate(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS images (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			uuid TEXT NOT NULL UNIQUE,
			original_name TEXT NOT NULL,
			filename TEXT NOT NULL,
			type TEXT NOT NULL CHECK(type IN ('desktop','mobile')),
			mime TEXT NOT NULL,
			extension TEXT NOT NULL,
			size INTEGER NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_images_type_filename ON images(type, filename)`,
		`CREATE INDEX IF NOT EXISTS idx_images_type_created ON images(type, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_images_type_id ON images(type, id)`,
		`CREATE TABLE IF NOT EXISTS sequences (
			type TEXT PRIMARY KEY,
			next_number INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS image_stats (
			type TEXT PRIMARY KEY,
			total INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO sequences(type, next_number) VALUES('desktop', 1)`,
		`INSERT OR IGNORE INTO sequences(type, next_number) VALUES('mobile', 1)`,
		/*
		 * 每次启动重算按分类计数：COUNT(*) 扫描 2 万行要 0.8ms，
		 * 放进列表接口太贵，所以运行时用事务内 ±1 维护，
		 * 启动时无条件重算一次，顺便修掉外部改库导致的漂移。
		 */
		`DELETE FROM image_stats`,
		`INSERT INTO image_stats(type, total) SELECT type, COUNT(*) FROM images GROUP BY type`,
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("database migration failed: %w", err)
		}
	}

	return nil
}
