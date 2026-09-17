package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Storage     StorageConfig     `yaml:"storage"`
	Auth        AuthConfig        `yaml:"auth"`
	Permissions PermissionsConfig `yaml:"permissions"`
	Limits      LimitsConfig      `yaml:"limits"`
	Upload      UploadConfig      `yaml:"upload"`
	Mobile      MobileConfig      `yaml:"mobile"`
}

type ServerConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	ReleaseMode bool   `yaml:"release_mode"`

	/*
	 * TrustedProxies 是可信反向代理的 IP / CIDR 列表。
	 *
	 * 安全语义：
	 *   - 只有当 TCP 连接来源（RemoteAddr）在这个列表里时，
	 *     才会解析 X-Forwarded-For / CF-Connecting-IP / X-Real-IP
	 *   - 否则这些头一律忽略，直接使用 TCP 来源 IP
	 *
	 * 这是防止"伪造 XFF 绕过 IP 配额 / 白名单"的关键。
	 *
	 * 留空时：所有转发头都被忽略，RemoteAddr 直接作为客户端 IP。
	 * 常见配置：
	 *   - 直接用 Cloudflare / Nginx 反代：填反代自己的 IP 或内网段
	 *   - 本机部署：127.0.0.1 / ::1
	 */
	TrustedProxies []string `yaml:"trusted_proxies"`

	/*
	 * ReadTimeoutSec / WriteTimeoutSec 是 HTTP 读写超时（秒）。
	 * 0 表示无超时（不推荐）。默认 600 秒（10 分钟）。
	 * 需要大于最大文件上传 / 下载所需的时间。
	 */
	ReadTimeoutSec  int `yaml:"read_timeout_sec"`
	WriteTimeoutSec int `yaml:"write_timeout_sec"`
}

type StorageConfig struct {
	DataDir    string `yaml:"data_dir"`
	StorageDir string `yaml:"storage_dir"`
}

type AuthConfig struct {
	AdminToken string `yaml:"admin_token"`
	GuestToken string `yaml:"guest_token"`

	/*
	 * SignKey 用于给 /images/... 的访问 URL 做 HMAC 签名。
	 *
	 * 如果 config.yaml 里留空，启动时会自动生成一个随机密钥，
	 * 此时进程重启后所有已签发的图片 URL 会立即失效。
	 *
	 * 生产环境请显式配置一个固定值，例如：
	 *   sign_key: "一个足够长的随机字符串"
	 */
	SignKey string `yaml:"sign_key"`

	AllowedIPs     []string `yaml:"allowed_ips"`
	AllowedOrigins []string `yaml:"allowed_origins"`
	GuardMode      string   `yaml:"guard_mode"`
}

type PermissionsConfig struct {
	Guest GuestPermissions `yaml:"guest"`
}

type GuestPermissions struct {
	Upload bool `yaml:"upload"`
	Delete bool `yaml:"delete"`
}

type LimitsConfig struct {
	Guest GuestLimits `yaml:"guest"`
}

type GuestLimits struct {
	APIOriginalPerDay      int `yaml:"api_original_per_day"`
	APIThumbPerDay         int `yaml:"api_thumb_per_day"`
	DownloadOriginalPerDay int `yaml:"download_original_per_day"`
	DownloadThumbPerDay    int `yaml:"download_thumb_per_day"`
}

type UploadConfig struct {
	MaxFileMB      int64 `yaml:"max_file_mb"`
	MaxBatchFiles  int   `yaml:"max_batch_files"`
	MaxBatchSizeMB int64 `yaml:"max_batch_size_mb"`
}

type MobileConfig struct {
	WidthThreshold int `yaml:"width_threshold"`
}

func Load() Config {
	cfg := defaultConfig()

	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "config.yaml"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[config] 读取 %s 失败: %v，使用默认配置\n", path, err)
		} else {
			fmt.Fprintf(os.Stderr, "[config] 未找到 %s，使用默认配置\n", path)
		}
		ensureSignKey(&cfg)
		return cfg
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "[config] 解析 %s 失败: %v，使用默认配置\n", path, err)
		cfg = defaultConfig()
		ensureSignKey(&cfg)
		return cfg
	}

	fmt.Fprintf(os.Stderr, "[config] 已加载 %s\n", path)
	ensureSignKey(&cfg)

	return cfg
}

func (c Config) Address() string {
	return c.Server.Host + ":" + strconv.Itoa(c.Server.Port)
}

/*
 * ensureSignKey 保证 SignKey 非空。
 *
 * 未配置时自动生成随机密钥，并打印警告。
 * 生产环境应显式在 config.yaml 里配置固定值，
 * 否则进程重启后旧的签名 URL 会全部失效。
 */
func ensureSignKey(cfg *Config) {
	if cfg.Auth.SignKey != "" {
		return
	}

	cfg.Auth.SignKey = randomSignKey()
	fmt.Fprintf(
		os.Stderr,
		"[config] 未配置 auth.sign_key，已自动生成随机密钥（进程重启后已签发的图片 URL 将失效）\n",
	)
}

func randomSignKey() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Host:            "0.0.0.0",
			Port:            2525,
			ReleaseMode:     true,
			TrustedProxies:  []string{"127.0.0.1", "::1"},
			ReadTimeoutSec:  600,
			WriteTimeoutSec: 600,
		},
		Storage: StorageConfig{
			DataDir:    "./data",
			StorageDir: "./storage",
		},
		Auth: AuthConfig{
			AdminToken:     "Hanbaka2550505",
			GuestToken:     "guest",
			SignKey:        "", // 留空 → 启动时自动生成随机密钥
			AllowedIPs:     []string{},
			AllowedOrigins: []string{},
			GuardMode:      "any",
		},
		Permissions: PermissionsConfig{
			Guest: GuestPermissions{
				Upload: false,
				Delete: false,
			},
		},
		Limits: LimitsConfig{
			Guest: GuestLimits{
				APIOriginalPerDay:      50,
				APIThumbPerDay:         0,
				DownloadOriginalPerDay: 10,
				DownloadThumbPerDay:    100,
			},
		},
		Upload: UploadConfig{
			MaxFileMB:      50,
			MaxBatchFiles:  100,
			MaxBatchSizeMB: 500,
		},
		Mobile: MobileConfig{
			WidthThreshold: 768,
		},
	}
}
