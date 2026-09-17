package middleware

import (
	"net"
	"strings"

	"github.com/gin-gonic/gin"
)

/*
 * ProxyResolver 决定一次请求的"真实客户端 IP"。
 *
 * 关键安全规则：
 *
 *   只有当 TCP 层的 RemoteAddr 命中 trusted_proxies 时，
 *   才会解析 X-Forwarded-For / CF-Connecting-IP / X-Real-IP。
 *
 *   否则这些头一律忽略，直接用 RemoteAddr。
 *
 * 这样攻击者不能通过伪造 XFF 来：
 *   - 绕过基于 IP 的每日配额
 *   - 伪造白名单 IP
 */
type ProxyResolver struct {
	exactIPs []net.IP
	ipNets   []*net.IPNet
}

func NewProxyResolver(trusted []string) *ProxyResolver {
	p := &ProxyResolver{}
	for _, raw := range trusted {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			if _, cidr, err := net.ParseCIDR(s); err == nil && cidr != nil {
				p.ipNets = append(p.ipNets, cidr)
			}
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			p.exactIPs = append(p.exactIPs, ip)
		}
	}
	return p
}

func (p *ProxyResolver) isTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, exact := range p.exactIPs {
		target := exact
		if t := exact.To4(); t != nil {
			target = t
		}
		if target.Equal(ip) {
			return true
		}
	}
	for _, cidr := range p.ipNets {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP 返回本次请求的真实客户端 IP。
func (p *ProxyResolver) ClientIP(c *gin.Context) string {
	remote := remoteIPFromCtx(c)

	/*
	 * TCP 来源不在可信代理列表里 → 完全忽略转发头。
	 *
	 * 这是防伪造的关键分支：攻击者直接连服务器时，
	 * 无论他伪造什么 XFF 都不会被采信。
	 */
	if !p.isTrusted(net.ParseIP(remote)) {
		if remote != "" {
			return remote
		}
		return c.Request.RemoteAddr
	}

	/* 可信代理来源：按优先级解析转发头 */

	if cf := strings.TrimSpace(c.GetHeader("CF-Connecting-IP")); cf != "" {
		if net.ParseIP(cf) != nil {
			return cf
		}
	}

	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		/*
		 * XFF 通常形如：client, proxy1, proxy2
		 * 从左往右找第一个不属于 trusted_proxies 的 IP，就是真实客户端。
		 */
		for _, part := range parts {
			candidate := strings.TrimSpace(part)
			if candidate == "" {
				continue
			}
			ip := net.ParseIP(candidate)
			if ip == nil {
				continue
			}
			if !p.isTrusted(ip) {
				return candidate
			}
		}
		/* 整条 XFF 都是可信代理 → 用最右一个作为来源 */
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}

	if rip := strings.TrimSpace(c.GetHeader("X-Real-IP")); rip != "" {
		if net.ParseIP(rip) != nil {
			return rip
		}
	}

	if remote != "" {
		return remote
	}
	return c.Request.RemoteAddr
}

func remoteIPFromCtx(c *gin.Context) string {
	addr := c.Request.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

/* =========================================================
   全局单例
   ========================================================= */

/*
 * ProxyResolver 是进程级的：代理列表在启动时固定。
 * 用一个全局变量避免把 resolver 穿透到每个 handler /
 * middleware 的构造参数里。
 */
var globalProxyResolver *ProxyResolver

// InitProxyResolver 在 main 启动时调用一次。
func InitProxyResolver(trusted []string) {
	globalProxyResolver = NewProxyResolver(trusted)
}

/*
 * GlobalClientIP 是"获取客户端真实 IP"的唯一入口。
 *
 * handler / middleware 都通过它取 IP，保证全项目一致。
 * 未初始化时退化为直接用 RemoteAddr（等价于"没有可信代理"）。
 */
func GlobalClientIP(c *gin.Context) string {
	if globalProxyResolver == nil {
		return remoteIPFromCtx(c)
	}
	return globalProxyResolver.ClientIP(c)
}
