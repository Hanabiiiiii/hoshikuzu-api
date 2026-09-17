package repository

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"random-image-api/internal/model"
)

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

func (r *Repository) Create(tx *sql.Tx, item *model.Image) error {
	_, err := tx.Exec(`
		INSERT INTO images(
			uuid, original_name, filename, type, mime, extension,
			size, width, height, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, item.UUID, item.OriginalName, item.Filename, item.Type, item.MIME,
		item.Extension, item.Size, item.Width, item.Height, item.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (r *Repository) FindByID(id int64) (*model.Image, error) {
	return scanImage(r.db.QueryRow(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
		FROM images WHERE id = ?
	`, id))
}

func (r *Repository) FindByFilename(imageType model.ImageType, filename string) (*model.Image, error) {
	return scanImage(r.db.QueryRow(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
		FROM images WHERE type = ? AND filename = ?
	`, imageType, filename))
}

func (r *Repository) List(imageType model.ImageType, offset, limit int) ([]model.Image, int64, error) {
	var total int64
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM images WHERE type = ?`, imageType).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
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

/*
 * Random 返回当前分类下的一张随机图片。
 *
 * 原实现用 ORDER BY RANDOM()，对每行生成随机数再排序，
 * 是 O(n) 全表扫描。图片量大时（几万张以上）会明显拖慢响应。
 *
 * 新实现：
 *  1. 拿到该分类的 min(id) / max(id)
 *  2. 在区间内随机取一个 id
 *  3. 向后/向前找第一条记录
 *
 * 走 (type, id) 索引，是 O(log n)。
 *
 * 注意：id 可能不连续（删除过图片），但用"找最近一条"解决。
 * 这是近似均匀随机，对随机壁纸场景完全够用。
 */
func (r *Repository) Random(imageType model.ImageType) (*model.Image, error) {
	var minID, maxID sql.NullInt64
	if err := r.db.QueryRow(
		`SELECT MIN(id), MAX(id) FROM images WHERE type = ?`,
		imageType,
	).Scan(&minID, &maxID); err != nil {
		return nil, err
	}
	if !minID.Valid || !maxID.Valid {
		return nil, sql.ErrNoRows
	}

	span := maxID.Int64 - minID.Int64 + 1

	var candidate int64
	if span <= 1 {
		candidate = minID.Int64
	} else {
		// rand.Int63n 是并发安全的，Go 1.20+ 自动播种
		candidate = minID.Int64 + rand.Int63n(span)
	}

	// 先往后找
	item, err := scanImage(r.db.QueryRow(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
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

	// 没找到说明候选 id 落在尾部空洞里，往前找
	return scanImage(r.db.QueryRow(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
		FROM images
		WHERE type = ? AND id < ?
		ORDER BY id DESC
		LIMIT 1
	`, imageType, candidate))
}

func (r *Repository) Delete(tx *sql.Tx, id int64) (*model.Image, error) {
	item, err := scanImage(tx.QueryRow(`
		SELECT id, uuid, original_name, filename, type, mime, extension,
		       size, width, height, created_at
		FROM images WHERE id = ?
	`, id))
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
