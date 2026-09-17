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
	if err := os.MkdirAll(cfg.Storage.DataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	for _, typ := range []model.ImageType{model.Desktop, model.Mobile} {
		dir := filepath.Join(cfg.Storage.StorageDir, string(typ))
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", typ, err)
		}
		thumbDir := filepath.Join(dir, "thumb")
		if err := os.MkdirAll(thumbDir, 0755); err != nil {
			return nil, fmt.Errorf("create %s/thumb directory: %w", typ, err)
		}
	}

	dbPath := filepath.Join(cfg.Storage.DataDir, "images.db")

	dsn := "file:" + dbPath + "?" + strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=cache_size(-20000)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=foreign_keys(ON)",
	}, "&")

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

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
		`INSERT OR IGNORE INTO sequences(type, next_number) VALUES('desktop', 1)`,
		`INSERT OR IGNORE INTO sequences(type, next_number) VALUES('mobile', 1)`,
	}

	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("database migration failed: %w", err)
		}
	}

	return nil
}
