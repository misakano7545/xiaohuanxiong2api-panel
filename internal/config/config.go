// Package config 读取 config.json 并把其中的键当作**环境变量的默认值**：
// 真实环境变量永远优先，文件只补空缺。这样 cmd/ 里所有 os.Getenv 的读取点都不用改，
// 面板/容器两条部署路径也各自成立（容器可以只给环境变量、没有配置文件）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Upstream 是小浣熊上游的地址与超时。
type Upstream struct {
	LLMBase        string `json:"llm_base"`
	AuthBase       string `json:"auth_base"`
	PointsBase     string `json:"points_base"`
	Origin         string `json:"origin"`
	UserAgent      string `json:"user_agent"`
	ConnectSeconds int    `json:"connect_timeout_seconds"`
	HeaderSeconds  int    `json:"header_timeout_seconds"`
	TotalSeconds   int    `json:"request_timeout_seconds"` // 0 = 不设总超时（长流式回答）
}

// Pool 是选号与冷却参数。
type Pool struct {
	MaxRotate          int `json:"max_rotate"`           // 单请求最多换几个号
	CooldownSeconds    int `json:"cooldown_seconds"`     // 429/5xx 后的冷却
	RefreshSkewSeconds int `json:"refresh_skew_seconds"` // 提前多久续期
}

// Schedule 是后台任务。
type Schedule struct {
	CheckinEnabled bool  `json:"checkin_enabled"`
	CheckinHours   []int `json:"checkin_hours"` // 每天几点尝试签到（本地时间）
	BalanceMinutes int   `json:"balance_refresh_minutes"`
}

// Config 是整份配置。
type Config struct {
	Listen       string   `json:"listen"`
	APIKey       string   `json:"api_key"`
	AdminKey     string   `json:"admin_key"`
	AuthDir      string   `json:"auth_dir"`
	DefaultModel string   `json:"default_model"`
	Models       []string `json:"models"` // /v1/models 的展示白名单；空 = 上游目录
	Upstream     Upstream `json:"upstream"`
	Pool         Pool     `json:"pool"`
	Schedule     Schedule `json:"schedule"`
}

// Default 返回可直接启动的默认配置。
func Default() Config {
	return Config{
		Listen:       "127.0.0.1:7867",
		AuthDir:      "./auths",
		DefaultModel: "raccoon-chat-ml-5-5",
		Upstream: Upstream{
			LLMBase:        "https://xiaohuanxiong.com/api/web/llm/v2",
			AuthBase:       "https://xiaohuanxiong.com/api/web/auth/v1",
			PointsBase:     "https://xiaohuanxiong.com/api/web/points/v1",
			Origin:         "https://xiaohuanxiong.com",
			UserAgent:      "xhx2api-panel/0.1.0",
			ConnectSeconds: 30,
			HeaderSeconds:  120,
		},
		Pool: Pool{MaxRotate: 3, CooldownSeconds: 600, RefreshSkewSeconds: 300},
		Schedule: Schedule{
			CheckinEnabled: true,
			CheckinHours:   []int{9},
			BalanceMinutes: 10,
		},
	}
}

// Load 读配置文件（可不存在）+ 环境变量覆盖，返回最终配置。
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return cfg, fmt.Errorf("读取配置文件 %s: %w", path, err)
		}
		if err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("解析配置文件 %s: %w", path, err)
			}
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.validate()
}

// applyEnv 用环境变量覆盖（空值跳过：面板保存留下的空串不代表"要清空"）。
func applyEnv(c *Config) {
	set := func(dst *string, key string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*dst = v
		}
	}
	setInt := func(dst *int, key string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	set(&c.Listen, "LISTEN")
	set(&c.APIKey, "API_KEY")
	set(&c.AdminKey, "ADMIN_KEY")
	set(&c.AuthDir, "AUTH_DIR")
	set(&c.DefaultModel, "DEFAULT_MODEL")
	if v := strings.TrimSpace(os.Getenv("MODELS")); v != "" {
		c.Models = splitList(v)
	}
	set(&c.Upstream.LLMBase, "LLM_BASE")
	set(&c.Upstream.AuthBase, "AUTH_BASE")
	set(&c.Upstream.PointsBase, "POINTS_BASE")
	set(&c.Upstream.Origin, "ORIGIN")
	set(&c.Upstream.UserAgent, "UPSTREAM_USER_AGENT")
	setInt(&c.Upstream.ConnectSeconds, "UPSTREAM_CONNECT_TIMEOUT_SECONDS")
	setInt(&c.Upstream.HeaderSeconds, "UPSTREAM_HEADER_TIMEOUT_SECONDS")
	setInt(&c.Upstream.TotalSeconds, "UPSTREAM_REQUEST_TIMEOUT_SECONDS")
	setInt(&c.Pool.MaxRotate, "POOL_MAX_ROTATE")
	setInt(&c.Pool.CooldownSeconds, "POOL_COOLDOWN_SECONDS")
	setInt(&c.Pool.RefreshSkewSeconds, "POOL_REFRESH_SKEW_SECONDS")
	setInt(&c.Schedule.BalanceMinutes, "BALANCE_REFRESH_MINUTES")
	if v := strings.TrimSpace(os.Getenv("CHECKIN_ENABLED")); v != "" {
		c.Schedule.CheckinEnabled = v == "1" || strings.EqualFold(v, "true")
	}
}

func splitList(v string) []string {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen 不能为空")
	}
	if c.Pool.MaxRotate <= 0 {
		c.Pool.MaxRotate = 1
	}
	if c.Upstream.LLMBase == "" || c.Upstream.AuthBase == "" || c.Upstream.Origin == "" {
		return fmt.Errorf("upstream 地址不完整（llm_base / auth_base / origin）")
	}
	return nil
}

// Cooldown 返回冷却时长。
func (c Config) Cooldown() time.Duration { return time.Duration(c.Pool.CooldownSeconds) * time.Second }

// RefreshSkew 返回 token 预刷新窗口。
func (c Config) RefreshSkew() time.Duration {
	return time.Duration(c.Pool.RefreshSkewSeconds) * time.Second
}
