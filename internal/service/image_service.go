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
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	randv2 "math/rand/v2"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/webp"
	"golang.org/x/image/draw"

	"random-image-api/internal/config"
	"random-image-api/internal/model"
	"random-image-api/internal/repository"
)

const maxPixels = 200_000_000

const (
	// 压缩图（mode=thumb）最长边上限：16:9 素材正好出 1920×1080，竖图不再放大。
	thumbMaxWidth  = 1920
	thumbQuality   = 82
	gridMaxWidth   = 480
	gridQuality    = 74
	// WebP 的质量刻度比 JPEG 宽松，gridQuality=74 对应 WebP 78：
	// 实测这个取值下网格图同时比 JPEG 更小、PSNR 更高。
	gridWebPQuality = 78
	thumbSkipSize  = 200 * 1024
	gridNamePrefix = "s_"
	webpMethod     = 2

	imageURLTTL = time.Hour
)

// 缩略图存在性是热路径（每页列表几十次 os.Stat），用进程内缓存摊平。
type derivativeSet struct {
	thumbJPEG string
	thumbWebP string
	gridJPEG  string
	gridWebP  string
}

type Service struct {
	cfg  config.Config
	db   *sql.DB
	repo *repository.Repository

	mu sync.Mutex

	thumbMu    sync.RWMutex
	thumbCache map[string]bool

	catalogMu sync.RWMutex
	catalog   map[model.ImageType][]repository.IndexEntry
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
	return &Service{
		cfg:        cfg,
		db:         db,
		repo:       repo,
		thumbCache: make(map[string]bool),
		catalog:    make(map[model.ImageType][]repository.IndexEntry),
	}
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

	if totalSize > s.cfg.Upload.MaxBatchSizeMB*1024*1024 {
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

// uploadOne 只在取号和写库时加锁，文件写入与缩略图编码并行进行。
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

	number, err := s.reserveNumber(imageType)
	if err != nil {
		return nil, fmt.Errorf("获取编号失败: %w", err)
	}

	filename := fmt.Sprintf("%d%s", number, info.Ext)
	dir := filepath.Join(s.cfg.Storage.StorageDir, string(imageType))

	if err := os.MkdirAll(filepath.Join(dir, "thumb"), 0755); err != nil {
		return nil, fmt.Errorf("创建图片目录失败: %w", err)
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

	derived := derivativeSet{}
	if written > thumbSkipSize {
		derived = s.derivativePaths(imageType, filename)
		if _, err := generateDerivatives(path, derived, s.webpQuality(), true); err != nil {
			log.Printf("[upload] 生成缩略图失败 %s: %v", filename, err)
			derived = derivativeSet{}
		}
	}

	record := &model.Image{
		UUID:         randomID(16),
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

	if err := s.insertRecord(record); err != nil {
		_ = os.Remove(path)
		derived.remove()
		return nil, fmt.Errorf("保存图片信息失败: %w", err)
	}

	s.catalogAdd(record)
	return record, nil
}

func (s *Service) reserveNumber(imageType model.ImageType) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	number, err := s.repo.NextNumber(tx, imageType)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return number, nil
}

func (s *Service) insertRecord(record *model.Image) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	id, err := s.repo.Create(tx, record)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	record.ID = id
	return nil
}

func (s *Service) FindByID(id int64) (*model.Image, error) {
	return s.repo.FindByID(id)
}

func (s *Service) FindByIDs(ids []int64) (map[int64]*model.Image, error) {
	return s.repo.FindByIDs(ids)
}

func (s *Service) FindByFilename(imageType model.ImageType, filename string) (*model.Image, error) {
	return s.repo.FindByFilename(imageType, filename)
}

func (s *Service) Random(imageType model.ImageType) (*model.Image, error) {
	// 命中内存目录时只按主键取一行，省掉两条边界查询；
	// 目录没命中（还没载入 / 该分类为空 / 外部改库）就退回纯数据库路径。
	if entry, ok := s.catalogPick(imageType); ok {
		if item, err := s.repo.FindByID(entry.ID); err == nil {
			return item, nil
		}
	}
	return s.repo.Random(imageType)
}

// RandomURL 只产出跳转目标：命中内存目录时一条查询都不发。
func (s *Service) RandomURL(imageType model.ImageType, preferWebP, useOriginal bool) (string, error) {
	item, ok := s.catalogPick(imageType)
	if !ok {
		var err error
		item, err = s.repo.Random(imageType)
		if err != nil {
			return "", err
		}
	}

	if useOriginal {
		return s.PublicURL(item), nil
	}
	return s.ThumbnailURL(item, preferWebP), nil
}

// LoadCatalog 把每个分类的 (id, 文件名) 清单读进内存。
// 这是进程内的缓存，和限流器 / 缩略图缓存一样基于「单进程独占数据库」的假设：
// 载入失败或被外部改库时不报错，直接退回数据库路径。
func (s *Service) LoadCatalog() {
	started := time.Now()
	total := 0

	for _, imageType := range []model.ImageType{model.Desktop, model.Mobile} {
		entries, err := s.repo.IndexEntries(imageType)
		if err != nil {
			log.Printf("[catalog] 载入 %s 清单失败: %v", imageType, err)
			continue
		}

		s.catalogMu.Lock()
		s.catalog[imageType] = entries
		s.catalogMu.Unlock()
		total += len(entries)
	}

	log.Printf("[catalog] 载入 %d 条图片清单，耗时 %s", total, time.Since(started).Round(time.Millisecond))
}

func (s *Service) catalogPick(imageType model.ImageType) (*model.Image, bool) {
	s.catalogMu.RLock()
	defer s.catalogMu.RUnlock()

	entries := s.catalog[imageType]
	if len(entries) == 0 {
		return nil, false
	}

	entry := entries[randv2.IntN(len(entries))]
	return &model.Image{ID: entry.ID, Filename: entry.Filename, Type: imageType}, true
}

func (s *Service) catalogAdd(item *model.Image) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	entries, ok := s.catalog[item.Type]
	if !ok {
		// 清单还没载入，启动时的 LoadCatalog 会带上这条
		return
	}
	s.catalog[item.Type] = append(entries, repository.IndexEntry{ID: item.ID, Filename: item.Filename})
}

func (s *Service) catalogRemove(items []*model.Image) {
	if len(items) == 0 {
		return
	}

	removed := make(map[int64]struct{}, len(items))
	for _, item := range items {
		removed[item.ID] = struct{}{}
	}

	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	for _, item := range items {
		entries, ok := s.catalog[item.Type]
		if !ok {
			continue
		}

		kept := entries[:0]
		for _, entry := range entries {
			if _, drop := removed[entry.ID]; !drop {
				kept = append(kept, entry)
			}
		}
		s.catalog[item.Type] = kept
	}
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

	return s.repo.List(imageType, (page-1)*size, size)
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

	s.removeFiles(record)
	s.catalogRemove([]*model.Image{record})
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

	for _, record := range records {
		s.removeFiles(record)
	}
	s.catalogRemove(records)

	return len(records), failed
}

func (s *Service) removeFiles(item *model.Image) {
	_ = os.Remove(s.FilePath(item))

	for _, path := range s.derivativePaths(item.Type, item.Filename).list() {
		_ = os.Remove(path)
		s.forgetPath(path)
	}
}

// CountDownloadMix 按单张下载的同一套规则统计批量下载要消耗哪种配额。
func (s *Service) CountDownloadMix(items []*model.Image, useOriginal bool) (thumbCount, originalCount int) {
	for _, item := range items {
		if useOriginal || (!s.HasThumbnail(item) && !s.IsSmallImage(item)) {
			originalCount++
			continue
		}
		thumbCount++
	}
	return
}

func (s *Service) CreateZip(items []*model.Image, useOriginal bool) (string, error) {
	if len(items) == 0 {
		return "", errors.New("没有找到可下载的图片")
	}
	if len(items) > s.cfg.Upload.MaxBatchFiles*2 {
		return "", fmt.Errorf("一次最多下载 %d 张图片", s.cfg.Upload.MaxBatchFiles*2)
	}

	sorted := make([]*model.Image, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Type == sorted[j].Type {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Type < sorted[j].Type
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

	zw := zip.NewWriter(file)
	buf := make([]byte, 64*1024)

	for _, item := range sorted {
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
			_ = file.Close()
			_ = os.Remove(zipPath)
			return "", err
		}

		if _, err := io.CopyBuffer(writer, input, buf); err != nil {
			_ = input.Close()
			_ = zw.Close()
			_ = file.Close()
			_ = os.Remove(zipPath)
			return "", err
		}
		_ = input.Close()
	}

	if err := zw.Close(); err != nil {
		_ = file.Close()
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
	return s.FilePathFromParts(item.Type, item.Filename)
}

func (s *Service) FilePathFromParts(imageType model.ImageType, filename string) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(imageType), filename)
}

func (s *Service) ThumbFilePathFromParts(imageType model.ImageType, filename string) string {
	return filepath.Join(s.cfg.Storage.StorageDir, string(imageType), "thumb", filename)
}

func (s *Service) ThumbnailPath(item *model.Image) string {
	return s.ThumbFilePathFromParts(item.Type, thumbnailFilename(item.Filename))
}

func (s *Service) GridThumbnailPath(item *model.Image) string {
	return s.ThumbFilePathFromParts(item.Type, gridThumbnailFilename(item.Filename))
}

func (s *Service) WebPThumbnailPath(item *model.Image) string {
	return s.ThumbFilePathFromParts(item.Type, webpFilename(item.Filename))
}

func (s *Service) WebPGridPath(item *model.Image) string {
	return s.ThumbFilePathFromParts(item.Type, gridWebPFilename(item.Filename))
}

// derivativePaths WebP 关闭时对应字段为空字符串，写入时会被跳过。
func (s *Service) derivativePaths(imageType model.ImageType, filename string) derivativeSet {
	set := derivativeSet{
		thumbJPEG: s.ThumbFilePathFromParts(imageType, thumbnailFilename(filename)),
		gridJPEG:  s.ThumbFilePathFromParts(imageType, gridThumbnailFilename(filename)),
	}
	if s.cfg.Thumbnails.WebP {
		set.thumbWebP = s.ThumbFilePathFromParts(imageType, webpFilename(filename))
		set.gridWebP = s.ThumbFilePathFromParts(imageType, gridWebPFilename(filename))
	}
	return set
}

func (d derivativeSet) list() []string {
	paths := make([]string, 0, 4)
	for _, path := range []string{d.thumbJPEG, d.thumbWebP, d.gridJPEG, d.gridWebP} {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (d derivativeSet) remove() {
	for _, path := range d.list() {
		_ = os.Remove(path)
	}
}

func (d derivativeSet) hasMissing() bool {
	for _, path := range d.list() {
		if _, err := os.Stat(path); err != nil {
			return true
		}
	}
	return false
}

// derivativeStale 检测已有缩略图是不是按更小的旧规格生成的。
// 这样改了 thumbMaxWidth（比如 1600 → 1920）之后，历史图片会自动重算，
// 不需要手动删文件或加开关。只看预览图的宽度：它和 WebP 是一起生成的。
func (s *Service) derivativeStale(item *model.Image, set derivativeSet) bool {
	if set.thumbJPEG == "" || item.Width <= 0 {
		return false
	}

	want := thumbMaxWidth
	if item.Width < want {
		want = item.Width
	}

	file, err := os.Open(set.thumbJPEG)
	if err != nil {
		return false
	}
	defer file.Close()

	cfg, _, err := image.DecodeConfig(file)
	if err != nil {
		return false
	}
	return cfg.Width < want
}

func (s *Service) webpQuality() int {
	quality := s.cfg.Thumbnails.WebPQuality
	if quality < 1 || quality > 100 {
		quality = 82
	}
	return quality
}

// 缩略图存在性是热路径（每页列表 30~60 次 os.Stat），用进程内缓存摊平。
func (s *Service) fileExists(path string) bool {
	s.thumbMu.RLock()
	exists, cached := s.thumbCache[path]
	s.thumbMu.RUnlock()
	if cached {
		return exists
	}

	_, err := os.Stat(path)
	exists = err == nil

	s.thumbMu.Lock()
	s.thumbCache[path] = exists
	s.thumbMu.Unlock()
	return exists
}

func (s *Service) forgetPath(path string) {
	s.thumbMu.Lock()
	delete(s.thumbCache, path)
	s.thumbMu.Unlock()
}

func (s *Service) HasThumbnail(item *model.Image) bool {
	return s.fileExists(s.ThumbnailPath(item))
}

func (s *Service) HasGridThumbnail(item *model.Image) bool {
	return s.fileExists(s.GridThumbnailPath(item))
}

func (s *Service) HasWebPThumbnail(item *model.Image) bool {
	if !s.cfg.Thumbnails.WebP {
		return false
	}
	return s.fileExists(s.WebPThumbnailPath(item))
}

func (s *Service) HasWebPGrid(item *model.Image) bool {
	if !s.cfg.Thumbnails.WebP {
		return false
	}
	return s.fileExists(s.WebPGridPath(item))
}

// 小于阈值的图片不生成缩略图，下载时返回原图但按压缩图配额计费。
func (s *Service) IsSmallImage(item *model.Image) bool {
	return item.Size <= int64(thumbSkipSize)
}

// BackfillDerivatives 为历史图片补齐缺失的派生文件（网格图 / WebP）。
// 每张图只解码一次原图，且跳过已存在的文件，所以可以反复在启动时跑。
func (s *Service) BackfillDerivatives(limit int) {
	if limit <= 0 {
		return
	}

	started := time.Now()
	processed := 0
	written := 0

	err := s.repo.EachImage(func(item *model.Image) bool {
		if processed >= limit {
			return false
		}
		if s.IsSmallImage(item) {
			return true
		}

		set := s.derivativePaths(item.Type, item.Filename)

		// 陈旧判断要和「缺文件」分开算：只缺 WebP 时如果不看尺寸，
		// 会把旧的 1600px JPEG 留下来，配上一张新的 1920px WebP。
		stale := s.derivativeStale(item, set)
		if !stale && !set.hasMissing() {
			return true
		}
		processed++

		count, err := generateDerivatives(s.FilePath(item), set, s.webpQuality(), stale)
		if err != nil {
			log.Printf("[derivative] 补齐 %s/%s 失败: %v", item.Type, item.Filename, err)
			return true
		}
		if count == 0 {
			return true
		}

		written += count
		for _, path := range set.list() {
			s.forgetPath(path)
		}
		return true
	})
	if err != nil {
		log.Printf("[derivative] 补齐派生文件中断: %v", err)
	}
	if processed > 0 {
		log.Printf("[derivative] 处理 %d 张历史图片，写出 %d 个文件，耗时 %s",
			processed, written, time.Since(started).Round(time.Millisecond))
	}
}

func (s *Service) SignPath(path string, ttl time.Duration) string {
	if s.cfg.Auth.SignKey == "" {
		return path
	}

	exp := time.Now().Add(ttl).Unix()
	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.SignKey))
	fmt.Fprintf(mac, "%s|%d", path, exp)

	return fmt.Sprintf("%s?exp=%d&sig=%s", path, exp, hex.EncodeToString(mac.Sum(nil)))
}

func (s *Service) VerifySignedPath(path string, exp int64, sig string) bool {
	if s.cfg.Auth.SignKey == "" {
		return true
	}
	if exp <= 0 || sig == "" || time.Now().Unix() > exp {
		return false
	}

	mac := hmac.New(sha256.New, []byte(s.cfg.Auth.SignKey))
	fmt.Fprintf(mac, "%s|%d", path, exp)
	expect := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expect), []byte(sig))
}

func (s *Service) PublicURL(item *model.Image) string {
	return s.SignPath(fmt.Sprintf("/images/%s/%s", item.Type, item.Filename), imageURLTTL)
}

// preferWebP 由调用方按请求的 Accept 头决定；对应的 WebP 文件不存在时自动回退 JPEG。
func (s *Service) ThumbnailURL(item *model.Image, preferWebP bool) string {
	if preferWebP && s.HasWebPThumbnail(item) {
		return s.SignPath(
			fmt.Sprintf("/images/%s/thumb/%s", item.Type, webpFilename(item.Filename)),
			imageURLTTL,
		)
	}
	if !s.HasThumbnail(item) {
		return s.PublicURL(item)
	}
	return s.SignPath(
		fmt.Sprintf("/images/%s/thumb/%s", item.Type, thumbnailFilename(item.Filename)),
		imageURLTTL,
	)
}

func (s *Service) GridURL(item *model.Image, preferWebP bool) string {
	if preferWebP && s.HasWebPGrid(item) {
		return s.SignPath(
			fmt.Sprintf("/images/%s/thumb/%s", item.Type, gridWebPFilename(item.Filename)),
			imageURLTTL,
		)
	}
	if !s.HasGridThumbnail(item) {
		return s.ThumbnailURL(item, preferWebP)
	}
	return s.SignPath(
		fmt.Sprintf("/images/%s/thumb/%s", item.Type, gridThumbnailFilename(item.Filename)),
		imageURLTTL,
	)
}

func (s *Service) DownloadURL(item *model.Image) string {
	return fmt.Sprintf("/api/images/%d/download", item.ID)
}

func thumbnailFilename(filename string) string {
	return strings.TrimSuffix(filename, filepath.Ext(filename)) + ".jpg"
}

func gridThumbnailFilename(filename string) string {
	return gridNamePrefix + thumbnailFilename(filename)
}

func webpFilename(filename string) string {
	return strings.TrimSuffix(thumbnailFilename(filename), ".jpg") + ".webp"
}

func gridWebPFilename(filename string) string {
	return gridNamePrefix + webpFilename(filename)
}

// generateDerivatives 解码一次原图，产出预览图 / 网格图的 JPEG 与 WebP。
// 必须从原图生成：从已有 JPEG 缩略图二次转码会掉 2 dB 左右质量，而且文件更大。
// 失败时只回滚本次新写入的文件，不碰预先存在的文件。
func generateDerivatives(srcPath string, set derivativeSet, webpQuality int, overwrite bool) (int, error) {
	in, err := os.Open(srcPath)
	if err != nil {
		return 0, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	img, _, err := image.Decode(in)
	if err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}

	created := make([]string, 0, 4)
	written := 0

	rollback := func(err error) (int, error) {
		for _, path := range created {
			_ = os.Remove(path)
		}
		return 0, err
	}

	outputs := []struct {
		path     string
		maxWidth int
		quality  int
		asWebP   bool
	}{
		{set.thumbJPEG, thumbMaxWidth, thumbQuality, false},
		{set.thumbWebP, thumbMaxWidth, webpQuality, true},
		{set.gridJPEG, gridMaxWidth, gridQuality, false},
		{set.gridWebP, gridMaxWidth, gridWebPQuality, true},
	}

	for _, out := range outputs {
		if out.path == "" {
			continue
		}
		if !overwrite {
			if _, err := os.Stat(out.path); err == nil {
				continue
			}
		}

		if err := writeScaledImage(img, out.path, out.maxWidth, out.quality, out.asWebP); err != nil {
			return rollback(err)
		}
		created = append(created, out.path)
		written++
	}

	return written, nil
}

func writeScaledImage(img image.Image, dstPath string, maxWidth, quality int, asWebP bool) error {
	dst := scaledImage(img, maxWidth)

	tmpPath := dstPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}

	if asWebP {
		err = webp.Encode(out, dst, webp.Options{Quality: quality, Method: webpMethod})
	} else {
		err = jpeg.Encode(out, dst, &jpeg.Options{Quality: quality})
	}
	if err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func scaledImage(img image.Image, maxWidth int) *image.RGBA {
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
	return dst
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

func inspectImage(reader io.ReadSeeker) (imageInfo, error) {
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		return imageInfo{}, fmt.Errorf("定位图片失败: %w", err)
	}

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
		return imageInfo{}, fmt.Errorf("图片分辨率过大（最大 %d 像素）", maxPixels)
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

	written, err := io.Copy(out, io.LimitReader(src, maxBytes+1))
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
