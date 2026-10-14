package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"random-image-api/internal/model"
)

const imageColumns = `id, uuid, original_name, filename, type, mime, extension,
	size, width, height, created_at`

type Repository struct {
	db *sql.DB
}

func New(db *sql.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) NextNumber(tx *sql.Tx, imageType model.ImageType) (int64, error) {
	var next int64
	if err := tx.QueryRow(`SELECT next_number FROM sequences WHERE type = ?`, imageType).Scan(&next); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO sequences(type, next_number) VALUES(?, 2)`, imageType); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if _, err := tx.Exec(`UPDATE sequences SET next_number = next_number + 1 WHERE type = ?`, imageType); err != nil {
		return 0, err
	}
	return next, nil
}

func (r *Repository) Create(tx *sql.Tx, item *model.Image) (int64, error) {
	result, err := tx.Exec(`
		INSERT INTO images(
			uuid, original_name, filename, type, mime, extension,
			size, width, height, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, item.UUID, item.OriginalName, item.Filename, item.Type, item.MIME,
		item.Extension, item.Size, item.Width, item.Height, item.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}

	if _, err := tx.Exec(`
		INSERT INTO image_stats(type, total) VALUES(?, 1)
		ON CONFLICT(type) DO UPDATE SET total = total + 1
	`, item.Type); err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

func (r *Repository) FindByID(id int64) (*model.Image, error) {
	return scanImage(r.db.QueryRow(`SELECT `+imageColumns+` FROM images WHERE id = ?`, id))
}

func (r *Repository) FindByFilename(imageType model.ImageType, filename string) (*model.Image, error) {
	return scanImage(r.db.QueryRow(
		`SELECT `+imageColumns+` FROM images WHERE type = ? AND filename = ?`,
		imageType, filename,
	))
}

func (r *Repository) FindByIDs(ids []int64) (map[int64]*model.Image, error) {
	found := make(map[int64]*model.Image, len(ids))
	if len(ids) == 0 {
		return found, nil
	}

	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}

	rows, err := r.db.Query(
		`SELECT `+imageColumns+` FROM images WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		item, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		found[item.ID] = item
	}
	return found, rows.Err()
}

func (r *Repository) EachImage(fn func(*model.Image) bool) error {
	rows, err := r.db.Query(`SELECT ` + imageColumns + ` FROM images ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		item, err := scanImage(rows)
		if err != nil {
			return err
		}
		if !fn(item) {
			break
		}
	}
	return rows.Err()
}

// IndexEntry 是随机取图目录用的最小字段集合。
// 只取两列,避免为了拿文件名把整行（含 11 列 + 时间解析）扫出来。
type IndexEntry struct {
	ID       int64
	Filename string
}

func (r *Repository) IndexEntries(imageType model.ImageType) ([]IndexEntry, error) {
	rows, err := r.db.Query(`SELECT id, filename FROM images WHERE type = ? ORDER BY id`, imageType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries := make([]IndexEntry, 0, 1024)
	for rows.Next() {
		var entry IndexEntry
		if err := rows.Scan(&entry.ID, &entry.Filename); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// total 读 image_stats 的 O(1) 计数；行缺失时退回 COUNT(*) 并把行补上。
func (r *Repository) total(imageType model.ImageType) (int64, error) {
	var total int64
	err := r.db.QueryRow(`SELECT total FROM image_stats WHERE type = ?`, imageType).Scan(&total)
	if err == nil {
		return total, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	if err := r.db.QueryRow(`SELECT COUNT(*) FROM images WHERE type = ?`, imageType).Scan(&total); err != nil {
		return 0, err
	}
	_, _ = r.db.Exec(`INSERT OR REPLACE INTO image_stats(type, total) VALUES(?, ?)`, imageType, total)
	return total, nil
}

func (r *Repository) List(imageType model.ImageType, offset, limit int) ([]model.Image, int64, error) {
	total, err := r.total(imageType)
	if err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(`
		SELECT `+imageColumns+`
		FROM images
		WHERE type = ?
		ORDER BY id DESC
		LIMIT ? OFFSET ?
	`, imageType, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]model.Image, 0, limit)
	for rows.Next() {
		item, err := scanImage(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// Random 走 (type, id) 索引，O(log n)，替代 ORDER BY RANDOM() 的全表扫描。
// id 区间可能有空洞，命中时向后/向前回退一条。
//
// MIN / MAX 必须拆成两条语句：SQLite 的 min/max 索引优化只对单个聚合生效，
// 写成 SELECT MIN(id), MAX(id) 会退化成整分区扫描（2 万张图实测 1.69ms → 36µs）。
func (r *Repository) Random(imageType model.ImageType) (*model.Image, error) {
	var minID, maxID sql.NullInt64

	if err := r.db.QueryRow(`SELECT MIN(id) FROM images WHERE type = ?`, imageType).Scan(&minID); err != nil {
		return nil, err
	}
	if !minID.Valid {
		return nil, sql.ErrNoRows
	}

	if err := r.db.QueryRow(`SELECT MAX(id) FROM images WHERE type = ?`, imageType).Scan(&maxID); err != nil {
		return nil, err
	}
	if !maxID.Valid || maxID.Int64 < minID.Int64 {
		return nil, sql.ErrNoRows
	}

	span := maxID.Int64 - minID.Int64 + 1
	candidate := minID.Int64
	if span > 1 {
		candidate += rand.Int63n(span)
	}

	item, err := scanImage(r.db.QueryRow(`
		SELECT `+imageColumns+`
		FROM images
		WHERE type = ? AND id >= ?
		ORDER BY id ASC
		LIMIT 1
	`, imageType, candidate))
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	return scanImage(r.db.QueryRow(`
		SELECT `+imageColumns+`
		FROM images
		WHERE type = ? AND id < ?
		ORDER BY id DESC
		LIMIT 1
	`, imageType, candidate))
}

func (r *Repository) Delete(tx *sql.Tx, id int64) (*model.Image, error) {
	item, err := scanImage(tx.QueryRow(`SELECT `+imageColumns+` FROM images WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}

	result, err := tx.Exec(`DELETE FROM images WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, fmt.Errorf("delete image %d failed", id)
	}

	result, err = tx.Exec(`UPDATE image_stats SET total = total - 1 WHERE type = ?`, item.Type)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		if _, err := tx.Exec(
			`INSERT INTO image_stats(type, total) SELECT type, COUNT(*) FROM images WHERE type = ? GROUP BY type`,
			item.Type,
		); err != nil {
			return nil, err
		}
	}

	return item, nil
}

func scanImage(row interface{ Scan(...any) error }) (*model.Image, error) {
	var item model.Image
	var typ string
	var created string

	if err := row.Scan(
		&item.ID,
		&item.UUID,
		&item.OriginalName,
		&item.Filename,
		&typ,
		&item.MIME,
		&item.Extension,
		&item.Size,
		&item.Width,
		&item.Height,
		&created,
	); err != nil {
		return nil, err
	}

	item.Type = model.ImageType(typ)
	parsed, err := time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = parsed
	return &item, nil
}
