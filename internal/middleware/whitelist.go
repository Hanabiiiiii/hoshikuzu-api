package middleware

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

/* =========================================================
   IP / Origin 白名单检查
   ========================================================= */

// IPOriginChecker 判断请求是否命中 IP / Origin 白名单。
type IPOriginChecker struct {
	exactIPs []net.IP
	ipNets   []*net.IPNet
	origins  map[string]struct{}
	mode     string
	disabled bool
}

func NewIPOriginChecker(allowedIPs, allowedOrigins []string, mode string) *IPOriginChecker {
	c := &IPOriginChecker{
		origins: make(map[string]struct{}),
	}

	for _, raw := range allowedIPs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			if _, cidr, err := net.ParseCIDR(s); err == nil && cidr != nil {
				c.ipNets = append(c.ipNets, cidr)
			}
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			c.exactIPs = append(c.exactIPs, ip)
		}
	}

	for _, raw := range allowedOrigins {
		s := strings.TrimSpace(strings.ToLower(raw))
		if s == "" {
			continue
		}
		c.origins[s] = struct{}{}
	}

	c.mode = strings.ToLower(strings.TrimSpace(mode))
	if c.mode != "all" {
		c.mode = "any"
	}

	c.disabled = len(c.exactIPs) == 0 && len(c.ipNets) == 0 && len(c.origins) == 0

	return c
}

// IsDisabled 白名单是否为空（空 = 未激活）。
func (c *IPOriginChecker) IsDisabled() bool {
	return c.disabled
}

/*
 * Allow 判断当前请求是否命中白名单。
 *
 * 核心语义：
 *   - 未配置的维度"不参与决策"，不能算作"通过"
 *   - 已配置的维度必须真实判断
 *
 * mode = "any"：任意一个已配置的维度通过 → 通过
 * mode = "all"：所有已配置的维度都通过 → 通过
 *
 * 无 Origin / Referer 的请求：
 *   - Origin 维度视为"不通过"
 *   - 如果同时配了 IP 白名单，仍可靠 IP 命中通过（any 模式）
 */
func (c *IPOriginChecker) Allow(ctx *gin.Context) bool {
	if c.disabled {
		return true
	}

	hasIPCheck := len(c.exactIPs) > 0 || len(c.ipNets) > 0
	hasOriginCheck := len(c.origins) > 0

	/* 计算已配置维度的真实结果 */
	ipOK := false
	if hasIPCheck {
		ipOK = checkIPAllowed(clientIPFromCtx(ctx), c.exactIPs, c.ipNets)
	}

	originOK := false
	if hasOriginCheck {
		origin := originFromCtx(ctx)
		if origin != "" {
			_, originOK = c.origins[origin]
		}
		/* origin == "" 时保持 false：无来源信息，Origin 维度不通过 */
	}

	/* 合并 */
	if c.mode == "all" {
		if hasIPCheck && !ipOK {
			return false
		}
		if hasOriginCheck && !originOK {
			return false
		}
		return true
	}

	/* any：任意一个已配置的维度通过即可 */
	if hasIPCheck && ipOK {
		return true
	}
	if hasOriginCheck && originOK {
		return true
	}
	return false
}

/* =========================================================
   临时密钥（6 位数字，15 分钟有效，失败 3 次锁定 1 小时）
   ========================================================= */

const (
	TempTokenTTL          = 15 * time.Minute
	TempTokenMaxFails     = 3
	TempTokenLockDuration = time.Hour
)

/*
 * TempTokenStore 管理一个单例临时管理员密钥。
 *
 * 安全特性：
 *   - 有效期 15 分钟，每次生成覆盖旧值
 *   - 每次验证失败累加计数
 *   - 连续失败 3 次 → 锁定 1 小时，期间无法验证也无法重新生成
 *   - 每次生成密钥会把失败计数归零
 *
 * 注意：Validate 采用"全局锁定"，不区分来源 IP。
 * 攻击者可以通过故意失败 3 次触发锁定，导致管理员 1 小时内
 * 无法生成新的临时密钥。如果这个 DoS 风险不可接受，
 * 可以把 failCount 改成按 IP 记录。
 */
type TempTokenStore struct {
	mu          sync.Mutex
	token       string
	expiresAt   time.Time
	failCount   int
	lockedUntil time.Time
}

func NewTempTokenStore() *TempTokenStore {
	return &TempTokenStore{}
}

/*
 * Generate 生成一个新密钥。
 *
 * 如果处于锁定期，返回 error。handler 应该把 error 转成 429。
 */
func (s *TempTokenStore) Generate() (string, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if time.Now().Before(s.lockedUntil) {
		remain := time.Until(s.lockedUntil)
		return "", 0, fmt.Errorf(
			"临时密钥已因连续 %d 次校验失败被锁定，请在 %s 后重试",
			TempTokenMaxFails,
			formatDuration(remain),
		)
	}

	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		n = big.NewInt(time.Now().UnixNano() % 1000000)
	}
	token := fmt.Sprintf("%06d", n.Int64())

	s.token = token
	s.expiresAt = time.Now().Add(TempTokenTTL)
	s.failCount = 0
	s.lockedUntil = time.Time{}

	return token, TempTokenTTL, nil
}

/*
 * Validate 校验一个候选密钥。
 *
 * 失败会累加计数；达到 TempTokenMaxFails 后进入锁定状态，
 * 密钥作废，直到 lockedUntil 之后才能 Generate。
 */
func (s *TempTokenStore) Validate(token string) bool {
	if token == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	/* 全局锁定：任何验证都直接拒绝 */
	if time.Now().Before(s.lockedUntil) {
		return false
	}

	if s.token == "" {
		return false
	}
	if time.Now().After(s.expiresAt) {
		s.token = ""
		return false
	}

	if SecureEqual(s.token, token) {
		return true
	}

	/* 失败：累加并检查是否触发锁定 */
	s.failCount++
	if s.failCount >= TempTokenMaxFails {
		s.lockedUntil = time.Now().Add(TempTokenLockDuration)
		s.token = ""
		s.failCount = 0
	}
	return false
}

/*
 * Remaining 返回当前密钥剩余有效期。
 * 锁定中或没有有效密钥时返回 0。
 */
func (s *TempTokenStore) Remaining() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token == "" || time.Now().After(s.expiresAt) {
		return 0
	}
	return time.Until(s.expiresAt)
}

/*
 * isTempTokenShape 判断 token 是不是"6 位纯数字"。
 *
 * 只有形状匹配时才会去 Validate，避免访客用普通 token
 * 访问时误触发临时密钥的失败计数。
 */
func isTempTokenShape(s string) bool {
	if len(s) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}

/* =========================================================
   中间件
   ========================================================= */

// WhitelistOnly 只允许白名单命中或白名单未激活的请求。
func WhitelistOnly(checker *IPOriginChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if checker.IsDisabled() {
			c.Next()
			return
		}

		if !checker.Allow(c) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"message": "当前 IP / 来源不在白名单，无法执行此操作",
			})
			return
		}

		c.Next()
	}
}

/* =========================================================
   工具
   ========================================================= */

/*
 * clientIPFromCtx 委托给 middleware.GlobalClientIP。
 *
 * 保证白名单 IP 匹配和配额 key 用的是同一份可信 IP，
 * 且转发头只在可信代理来源时才被解析。
 */
func clientIPFromCtx(c *gin.Context) string {
	return GlobalClientIP(c)
}

func checkIPAllowed(raw string, exactIPs []net.IP, ipNets []*net.IPNet) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return false
	}

	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}

	for _, exact := range exactIPs {
		target := exact
		if t := exact.To4(); t != nil {
			target = t
		}
		if target.Equal(ip) {
			return true
		}
	}

	for _, cidr := range ipNets {
		if cidr.Contains(ip) {
			return true
		}
	}

	return false
}

func originFromCtx(c *gin.Context) string {
	if origin := strings.TrimSpace(c.GetHeader("Origin")); origin != "" {
		return normalizeOrigin(origin)
	}

	referer := strings.TrimSpace(c.GetHeader("Referer"))
	if referer == "" {
		return ""
	}

	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}

	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func normalizeOrigin(origin string) string {
	origin = strings.TrimSpace(strings.ToLower(origin))
	origin = strings.TrimSuffix(origin, "/")

	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return origin
	}

	return u.Scheme + "://" + u.Host
}
