package service

import (
	"archive/zip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // 保留：让 image.DecodeConfig 能识别 GIF 并给出友好错误
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
	"random-image-api/internal/config"
	"random-image-api/internal/model"
	"random-image-api/internal/repository"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

const maxPixels = 200_000_000

const (
	thumbMaxWidth = 1920
	thumbQuality  = 82
	thumbSkipSize = 200 * 1024

	imageURLTTL = time.Hour
)

type Service struct {
	cfg  config.Config
	db   *sql.DB
	repo *repository.Repository
	mu   sync.Mutex
}

type imageInfo struct {
	Width  int
	Height int
	MIME   string
	Ext    string
}

type imageFormat struct {
	MIME string
	Ext  string
}

var (
	formatJPEG = imageFormat{MIME: "image/jpeg", Ext: ".jpg"}
	formatPNG  = imageFormat{MIME: "image/png", Ext: ".png"}
	formatWEBP = imageFormat{MIME: "image/webp", Ext: ".webp"}
)

func New(cfg config.Config, db *sql.DB, repo *repository.Repository) *Service {
	return &Service{cfg: cfg, db: db, repo: repo}
}

func (s *Service) Upload(file *multipart.FileHeader) (*model.Image, error) {
	return s.uploadOne(file)
}

func (s *Service) UploadBatch(files []*multipart.FileHeader) ([]model.Image, []string) {
	uploaded := make([]model.Image, 0, len(files))
	failed := make([]string, 0)

	if len(files) == 0 {
		return uploaded, []string{"没有收到任何图片"}
	}
	if len(files) > s.cfg.Upload.MaxBatchFiles {
		return uploaded, []string{fmt.Sprintf("一次最多上传 %d 张图片", s.cfg.Upload.MaxBatchFiles)}
	}

	var totalSize int64
	for _, file := range files {
		if file != nil {
			totalSize += file.Size
		}
	}

	maxBatchSize := s.cfg.Upload.MaxBatchSizeMB * 1024 * 1024
	if totalSize > maxBatchSize {
		return uploaded, []string{fmt.Sprintf("本次总大小不能超过 %d MB", s.cfg.Upload.MaxBatchSizeMB)}
	}

	for _, file := range files {
		if file == nil {
			failed = append(failed, "unknown: 文件为空")
			continue
		}

		item, err := s.uploadOne(file)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", filepath.Base(file.Filename), err))
			continue
		}
		uploaded = append(uploaded, *item)
	}

	return uploaded, failed
}

func (s *Service) uploadOne(fileHeader *multipart.FileHeader) (*model.Image, error) {
	if fileHeader == nil {
		return nil, errors.New("文件为空")
	}
	if fileHeader.Size <= 0 {
		return nil, errors.New("文件为空")
	}

	maxSize := s.cfg.Upload.MaxFileMB * 1024 * 1024
	if fileHeader.Size > maxSize {
		return nil, fmt.Errorf("单张图片不能超过 %d MB", s.cfg.Upload.MaxFileMB)
	}

	file, err := fileHeader.Open()
	if err != nil {
		return nil, fmt.Errorf("打开图片失败: %w", err)
	}
	defer file.Close()

	info, err := inspectImage(file)
	if err != nil {
		return nil, err
	}

	imageType := classifyImage(info.Width, info.Height, s.cfg.Mobile.WidthThreshold)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("开始数据库事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	number, err := s.repo.NextNumber(tx, imageType)
	if err != nil {
		return nil, fmt.Errorf("获取编号失败: %w", err)
	}

	filename := fmt.Sprintf("%d%s", number, info.Ext)
	uuid := randomID(16)

	dir := filepath.Join(s.cfg.Storage.StorageDir, string(imageType))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建图片目录失败: %w", err)
	}

	thumbDir := filepath.Join(dir, "thumb")
	if err := os.MkdirAll(thumbDir, 0755); err != nil {
		return nil, fmt.Errorf("创建缩略图目录失败: %w", err)
	}

	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("目标文件已存在: %s", filename)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("检查目标文件失败: %w", err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("读取图片失败: %w", err)
	}

	written, err := saveAtomic(path, file, maxSize)
	if err != nil {
		return nil, err
	}

	thumbPath := filepath.Join(thumbDir, thumbnailFilename(filename))

	/*
	 * 缩略图生成策略：
	 *
	 *   - 原文件超过 thumbSkipSize（200 KB）时生成缩略图。
	 *   - 小于阈值的图片不生成缩略图，由 ThumbnailURL() 回退到原图，
	 *     下载时也返回原图，但按"压缩图"配额计费（小图不占原图配额）。
	 *
	 * GIF 已被拒绝上传，所以这里不再需要跳过动图。
	 */
	if written > thumbSkipSize {
		if err := generateThumbnail(path, thumbPath, thumbMaxWidth, thumbQuality); err != nil {
			log.Printf("[upload] generate thumbnail for %s failed: %v", filename, err)
		}
	}

	record := &model.Image{
		UUID:         uuid,
		OriginalName: filepath.Base(fileHeader.Filename),
		Filename:     filename,
		Type:         imageType,
		MIME:         info.MIME,
		Extension:    info.Ext,
		Size:         written,
		Width:        info.Width,
		Height:       info.Height,
		CreatedAt:    time.Now().UTC(),
	}

	if err := s.repo.Create(tx, record); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(thumbPath)
		return nil, fmt.Errorf("保存图片信息失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		_ = os.Remove(path)
		_ = os.Remove(thumbPath)
		return nil, fmt.Errorf("提交数据库事务失败: %w", err)
	}
	committed = true

	return record, nil
}

func (s *Service) FindByID(id int64) (*model.Image, error) {
	return s.repo.FindByID(id)
}

func (s *Service) FindByFilename(imageType model.ImageType, filename string) (*model.Image, error) {
	return s.repo.FindByFilename(imageType, filename)
}

func (s *Service) Random(imageType model.ImageType) (*model.Image, error) {
	return s.repo.Random(imageType)
}

func (s *Service) List(imageType model.ImageType, page, size int) ([]model.Image, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 30
	}
	if size > 200 {
		size = 200
	}

	offset := (page - 1) * size
	return s.repo.List(imageType, offset, size)
}

func (s *Service) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开始删除事务失败: %w", err)
	}
	defer tx.Rollback()

	record, err := s.repo.Delete(tx, id)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交删除事务失败: %w", err)
	}

	_ = os.Remove(s.FilePath(record))
	_ = os.Remove(s.ThumbnailPath(record))

	return nil
}

func (s *Service) DeleteBatch(ids []int64) (int, []string) {
	if len(ids) == 0 {
		return 0, []string{"没有提供图片 ID"}
	}
	if len(ids) > s.cfg.Upload.MaxBatchFiles*2 {
		return 0, []string{fmt.Sprintf("一次最多删除 %d 张图片", s.cfg.Upload.MaxBatchFiles*2)}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, []string{fmt.Sprintf("开始删除事务失败: %v", err)}
	}
	defer tx.Rollback()

	seen := make(map[int64]struct{}, len(ids))
	records := make([]*model.Image, 0, len(ids))
	failed := make([]string, 0)

	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		record, err := s.repo.Delete(tx, id)
		if err != nil {
			failed = append(failed, fmt.Sprintf("ID %d: %v", id, err))
			continue
		}
		records = append(records, record)
	}

	if err := tx.Commit(); err != nil {
		return 0, append(failed, fmt.Sprintf("提交删除事务失败: %v", err))
	}

	deleted := 0
	for _, record := range records {
		_ = os.Remove(s.FilePath(record))
		_ = os.Remove(s.ThumbnailPath(record))
		deleted++
	}

	return deleted, failed
}

func (s *Service) CreateZip(ids []int64, useOriginal bool) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("没有提供图片 ID")
	}
	if len(ids) > s.cfg.Upload.MaxBatchFiles*2 {
		return "", fmt.Errorf("一次最多下载 %d 张图片", s.cfg.Upload.MaxBatchFiles*2)
	}

	seen := make(map[int64]struct{}, len(ids))
	items := make([]*model.Image, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		item, err := s.FindByID(id)
		if err != nil {
			continue
		}
		items = append(items, item)
	}

	if len(items) == 0 {
		return "", errors.New("没有找到可下载的图片")
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Type == items[j].Type {
			return items[i].ID < items[j].ID
		}
		return items[i].Type < items[j].Type
	})

	tmpDir := filepath.Join(s.cfg.Storage.DataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", err
	}

	zipPath := filepath.Join(
		tmpDir,
		fmt.Sprintf("random-images-%d-%s.zip", time.Now().UnixNano(), randomID(4)),
	)

	file, err := os.Create(zipPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	zw := zip.NewWriter(file)

	for _, item := range items {
		srcPath := s.FilePath(item)
		entryName := filepath.ToSlash(filepath.Join(string(item.Type), item.Filename))

		if !useOriginal {
			thumbPath := s.ThumbnailPath(item)
			if _, err := os.Stat(thumbPath); err == nil {
				srcPath = thumbPath
				entryName = filepath.ToSlash(filepath.Join(string(item.Type), "thumb", thumbnailFilename(item.Filename)))
			}
		}

		input, err := os.Open(srcPath)
		if err != nil {
			continue
		}

		writer, err := zw.Create(entryName)
		if err != nil {
			_ = input.Close()
			_ = zw.Close()
			_ = os.Remove(zipPath)
			return "", err
		}

		if _, err := io.Copy(writer, input); err != nil {
			_ = input.Close()
			_ = zw.Close()
			_ = os.Remove(zipPath)
			return "", err
		}
		_ = input.Close()
	}

	if err := zw.Close(); err != nil {
		_ = os.Remove(zipPath)
		return "", err
	}

	if err := file.Close(); err != nil {
		_ = os.Remove(zipPath)
		return "", err
	}

	return zipPath, nil
}

func (s *Service) FilePath(item *model.Image) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(item.Type), item.Filename)
}

func (s *Service) FilePathFromParts(imageType model.ImageType, filename string) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(imageType), filename)
}

func (s *Service) ThumbFilePathFromParts(imageType model.ImageType, filename string) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(imageType), "thumb", filename)
}

func (s *Service) ThumbnailPath(item *model.Image) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(item.Type), "thumb", thumbnailFilename(item.Filename))
}

/*
 * HasThumbnail 判断缩略图文件是否存在。
 *
 * 小图（Size ≤ thumbSkipSize）上传时不会生成缩略图，
 * 因此这里会返回 false。
 */
func (s *Service) HasThumbnail(item *model.Image) bool {
	thumbPath := s.ThumbnailPath(item)
	_, err := os.Stat(thumbPath)
	return err == nil
}

/*
 * IsSmallImage 判断图片是否小于缩略图生成阈值。
 *
 * 这类图片上传时不会生成缩略图，下载时返回原图，
 * 但仍按"压缩图"配额计费 —— 小文件不占用宝贵的原图配额。
 */
func (s *Service) IsSmallImage(item *model.Image) bool {
	return item.Size <= int64(thumbSkipSize)
}

/* =========================================================
   签名 URL
   ========================================================= */

func (s *Service) SignPath(path string, ttl time.Duration) string {
	if s.cfg.Auth.SignKey == "" {
		return path
	}

	exp := time.Now().Add(ttl).Unix()
	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.SignKey))
	fmt.Fprintf(mac, "%s|%d", path, exp)
	sig := hex.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, sig)
}

func (s *Service) VerifySignedPath(path string, exp int64, sig string) bool {
	if s.cfg.Auth.SignKey == "" {
		return true
	}
	if exp <= 0 || sig == "" {
		return false
	}
	if time.Now().Unix() > exp {
		return false
	}

	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.SignKey))
	fmt.Fprintf(mac, "%s|%d", path, exp)
	expect := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expect), []byte(sig))
}

func (s *Service) PublicURL(item *model.Image) string {
	path := fmt.Sprintf("/images/%s/%s", item.Type, item.Filename)
	return s.SignPath(path, imageURLTTL)
}

func (s *Service) ThumbnailURL(item *model.Image) string {
	thumbPath := s.ThumbnailPath(item)
	if _, err := os.Stat(thumbPath); err != nil {
		return s.PublicURL(item)
	}

	path := fmt.Sprintf("/images/%s/thumb/%s", item.Type, thumbnailFilename(item.Filename))
	return s.SignPath(path, imageURLTTL)
}

func (s *Service) DownloadURL(item *model.Image) string {
	return fmt.Sprintf("/api/images/%d/download", item.ID)
}

func thumbnailFilename(filename string) string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	return base + ".jpg"
}

func generateThumbnail(srcPath, dstPath string, maxWidth, quality int) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	img, _, err := image.Decode(in)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	newW, newH := w, h
	if w > maxWidth {
		newW = maxWidth
		newH = h * maxWidth / w
		if newH < 1 {
			newH = 1
		}
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	if newW != w || newH != h {
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	} else {
		draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Over)
	}

	out, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer out.Close()

	return jpeg.Encode(out, dst, &jpeg.Options{Quality: quality})
}

func classifyImage(width, height, mobileWidth int) model.ImageType {
	if width <= 0 || height <= 0 {
		return model.Desktop
	}

	if height >= width || width <= mobileWidth {
		return model.Mobile
	}

	return model.Desktop
}

// ======================================================
// 图片内容校验
// ======================================================

func inspectImage(reader io.ReadSeeker) (imageInfo, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return imageInfo{}, fmt.Errorf("定位图片失败: %w", err)
	}

	if _, err := reader.Seek(0, io.SeekStart); err == nil {
		cfg, formatName, decodeErr := image.DecodeConfig(reader)
		if decodeErr == nil {
			format, ok := formatFromDecodeName(formatName)
			if !ok {
				return imageInfo{}, fmt.Errorf(
					"不支持的图片格式：%s，仅支持 JPG/JPEG、PNG、WEBP（不支持 GIF）",
					formatName,
				)
			}
			return buildImageInfo(cfg.Width, cfg.Height, format)
		}
	}

	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return imageInfo{}, fmt.Errorf("定位图片失败: %w", err)
	}

	header := make([]byte, 512)
	n, err := io.ReadFull(reader, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return imageInfo{}, fmt.Errorf("读取图片头失败: %w", err)
	}
	if n == 0 {
		return imageInfo{}, errors.New("图片文件为空")
	}
	header = header[:n]

	format, ok := detectFormat(header)
	if !ok {
		return imageInfo{}, errors.New("不支持的图片格式，仅支持 JPG/JPEG、PNG、WEBP（不支持 GIF）")
	}

	if _, err := reader.Seek(0, io.SeekStart); err == nil {
		width, height, parseErr := readDimensionsManual(reader, header, format)
		if parseErr == nil {
			return buildImageInfo(width, height, format)
		}
	}

	return imageInfo{}, errors.New("无法解析图片内容，文件可能已损坏或不是有效的图片")
}

func formatFromDecodeName(name string) (imageFormat, bool) {
	switch name {
	case "jpeg":
		return formatJPEG, true
	case "png":
		return formatPNG, true
	case "webp":
		return formatWEBP, true
	}
	return imageFormat{}, false
}

func detectFormat(header []byte) (imageFormat, bool) {
	if len(header) >= 3 &&
		header[0] == 0xFF &&
		header[1] == 0xD8 &&
		header[2] == 0xFF {
		return formatJPEG, true
	}

	if len(header) >= 8 && string(header[:8]) == "\x89PNG\r\n\x1a\n" {
		return formatPNG, true
	}

	if len(header) >= 12 &&
		string(header[:4]) == "RIFF" &&
		string(header[8:12]) == "WEBP" {
		return formatWEBP, true
	}

	return imageFormat{}, false
}

func readDimensionsManual(reader io.Reader, header []byte, format imageFormat) (int, int, error) {
	switch format.MIME {
	case formatJPEG.MIME:
		return readJPEGDimension(reader)

	case formatPNG.MIME:
		if len(header) >= 24 && string(header[12:16]) == "IHDR" {
			width := int(binary.BigEndian.Uint32(header[16:20]))
			height := int(binary.BigEndian.Uint32(header[20:24]))
			return width, height, nil
		}
		return 0, 0, errors.New("PNG 缺少 IHDR 块")

	case formatWEBP.MIME:
		return readWebPDimension(reader)
	}

	return 0, 0, errors.New("不支持的图片格式")
}

func buildImageInfo(width, height int, format imageFormat) (imageInfo, error) {
	if width <= 0 || height <= 0 {
		return imageInfo{}, errors.New("图片尺寸无效")
	}

	if int64(width)*int64(height) > maxPixels {
		return imageInfo{}, fmt.Errorf(
			"图片分辨率过大（最大 %d 像素）",
			maxPixels,
		)
	}

	return imageInfo{
		Width:  width,
		Height: height,
		MIME:   format.MIME,
		Ext:    format.Ext,
	}, nil
}

func readJPEGDimension(reader io.Reader) (int, int, error) {
	data := make([]byte, 2)
	if _, err := io.ReadFull(reader, data); err != nil {
		return 0, 0, err
	}
	if data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0, errors.New("无效 JPEG")
	}

	for {
		if _, err := io.ReadFull(reader, data); err != nil {
			return 0, 0, err
		}

		for data[0] != 0xFF {
			data[0] = data[1]
			if _, err := io.ReadFull(reader, data[1:2]); err != nil {
				return 0, 0, err
			}
		}

		marker := data[1]
		for marker == 0xFF {
			if _, err := io.ReadFull(reader, data[1:2]); err != nil {
				return 0, 0, err
			}
			marker = data[1]
		}

		if marker == 0xD8 || marker == 0xD9 {
			continue
		}
		if marker == 0xDA {
			return 0, 0, errors.New("JPEG 缺少尺寸信息")
		}

		if _, err := io.ReadFull(reader, data); err != nil {
			return 0, 0, err
		}

		segmentLength := int(binary.BigEndian.Uint16(data))
		if segmentLength < 2 {
			return 0, 0, errors.New("无效 JPEG 段")
		}

		isSOF := (marker >= 0xC0 && marker <= 0xC3) ||
			(marker >= 0xC5 && marker <= 0xC7) ||
			(marker >= 0xC9 && marker <= 0xCB) ||
			(marker >= 0xCD && marker <= 0xCF)

		if isSOF {
			sof := make([]byte, 5)
			if _, err := io.ReadFull(reader, sof); err != nil {
				return 0, 0, err
			}
			height := int(binary.BigEndian.Uint16(sof[1:3]))
			width := int(binary.BigEndian.Uint16(sof[3:5]))
			if width > 0 && height > 0 {
				return width, height, nil
			}
			return 0, 0, errors.New("JPEG 尺寸无效")
		}

		if segmentLength > 2 {
			if _, err := io.CopyN(io.Discard, reader, int64(segmentLength-2)); err != nil {
				return 0, 0, err
			}
		}
	}
}

func readWebPDimension(reader io.Reader) (int, int, error) {
	header := make([]byte, 40)
	n, err := io.ReadFull(reader, header)
	if err != nil && n < 30 {
		return 0, 0, errors.New("WEBP 文件头不完整")
	}
	if string(header[:4]) != "RIFF" || string(header[8:12]) != "WEBP" {
		return 0, 0, errors.New("无效 WEBP 文件")
	}

	switch string(header[12:16]) {
	case "VP8 ":
		if n < 30 || header[23] != 0x9D || header[24] != 0x01 || header[25] != 0x2A {
			return 0, 0, errors.New("无效 WEBP VP8")
		}
		width := int(uint16(header[26]) | uint16(header[27])<<8)
		height := int(uint16(header[28]) | uint16(header[29])<<8)
		return width, height, nil

	case "VP8L":
		if n < 25 || header[20] != 0x2F {
			return 0, 0, errors.New("无效 WEBP VP8L")
		}
		b0 := uint32(header[21])
		b1 := uint32(header[22])
		b2 := uint32(header[23])
		b3 := uint32(header[24])
		width := 1 + int((b0|b1<<8|b2<<16)&0x3FFF)
		height := 1 + int((b1>>6|b2<<2|b3<<10)&0x3FFF)
		return width, height, nil

	case "VP8X":
		if n < 30 {
			return 0, 0, errors.New("无效 WEBP VP8X")
		}
		width := 1 + int(uint32(header[24])|uint32(header[25])<<8|uint32(header[26])<<16)
		height := 1 + int(uint32(header[27])|uint32(header[28])<<8|uint32(header[29])<<16)
		return width, height, nil

	default:
		return 0, 0, errors.New("不支持的 WEBP 编码")
	}
}

func saveAtomic(path string, src io.Reader, maxBytes int64) (int64, error) {
	tmpPath := path + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return 0, fmt.Errorf("创建临时文件失败: %w", err)
	}

	limited := io.LimitReader(src, maxBytes+1)

	written, err := io.Copy(out, limited)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("保存图片失败: %w", err)
	}
	if written > maxBytes {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("图片大小超过 %d 字节", maxBytes)
	}

	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("同步图片失败: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("关闭图片失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("保存图片失败: %w", err)
	}

	return written, nil
}

func randomID(length int) string {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.ToLower(fmt.Sprintf("%x", buf))
}
