package middleware

import (
	"sync"
	"time"
)

/*
 * RateLimiter 是进程内的每日配额。
 *
 * 单实例场景足够；多实例部署时需要换成 Redis。
 *
 * key 示例：
 *   api-original:1.2.3.4
 *   download-original:1.2.3.4
 *
 * 每天的 key 会带上日期，跨天自动重置。
 */
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rateEntry
}

type rateEntry struct {
	count int
	date  string
}

func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{entries: make(map[string]*rateEntry)}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.cleanup()
	}
}

func (rl *RateLimiter) cleanup() {
	today := time.Now().Format("2006-01-02")
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, v := range rl.entries {
		if v.date != today {
			delete(rl.entries, k)
		}
	}
}

/*
 * Allow 检查是否可以再消耗 1 次配额。
 *
 * 返回：
 *   allowed 是否允许
 *   used    本次消耗后的计数（或超限时的当前计数）
 */
func (rl *RateLimiter) Allow(key string, limit int) (bool, int) {
	return rl.AllowN(key, limit, 1)
}

/*
 * AllowN 一次消耗 N 次配额（用于批量下载原图）。
 *
 * 逻辑：
 *   - 如果剩余配额 >= N，全部扣掉
 *   - 否则一个都不扣，返回 false
 */
func (rl *RateLimiter) AllowN(key string, limit, n int) (bool, int) {
	if n < 0 {
		n = 0
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	today := time.Now().Format("2006-01-02")

	entry, ok := rl.entries[key]
	if !ok || entry.date != today {
		if n > limit {
			return false, 0
		}
		rl.entries[key] = &rateEntry{count: n, date: today}
		return true, n
	}

	if entry.count+n > limit {
		return false, entry.count
	}

	entry.count += n
	return true, entry.count
}

/*
 * Remaining 查询剩余配额。
 */
func (rl *RateLimiter) Remaining(key string, limit int) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	today := time.Now().Format("2006-01-02")

	entry, ok := rl.entries[key]
	if !ok || entry.date != today {
		return limit
	}

	remaining := limit - entry.count
	if remaining < 0 {
		return 0
	}
	return remaining
}
