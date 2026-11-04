package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"dbname"`
}

func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=Local&timeout=5s&readTimeout=10s&writeTimeout=10s",
		d.User, d.Password, d.Host, d.Port, d.DBName)
}

type GroupConfig struct {
	ChatID      int64          `yaml:"chat_id"`
	Database    DatabaseConfig `yaml:"database"`
	ExemptUsers []int64        `yaml:"exempt_users"`
	// VerifyTimeout 验证超时（秒），默认 300
	VerifyTimeout int `yaml:"verify_timeout"`
}

func (g GroupConfig) IsExempt(telegramID int64) bool {
	for _, id := range g.ExemptUsers {
		if id == telegramID {
			return true
		}
	}
	return false
}

type BotCommand struct {
	Command     string `yaml:"command"`
	Description string `yaml:"description"`
}

type BotProfileConfig struct {
	Description      string       `yaml:"description"`
	ShortDescription string       `yaml:"short_description"`
	Commands         []BotCommand `yaml:"commands"`
}

type TelegramConfig struct {
	BotToken string           `yaml:"bot_token"`
	AdminIDs []int64          `yaml:"admin_ids"`
	Profile  BotProfileConfig `yaml:"profile"`
}

type Config struct {
	Telegram      TelegramConfig `yaml:"telegram"`
	Groups        []GroupConfig  `yaml:"groups"`
	CheckInterval int            `yaml:"check_interval"`
	// CacheSize 每个群已验证用户的 LRU 缓存上限，0 表示使用默认值 5000
	CacheSize int `yaml:"cache_size"`
}

func (c Config) IsAdmin(telegramID int64) bool {
	for _, id := range c.Telegram.AdminIDs {
		if id == telegramID {
			return true
		}
	}
	return false
}

// Load 从 YAML 文件加载配置
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if cfg.Telegram.BotToken == "" {
		return nil, fmt.Errorf("bot_token 不能为空")
	}
	if len(cfg.Groups) == 0 {
		return nil, fmt.Errorf("至少需要配置一个群组")
	}
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 300
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 5000
	}

	for i, g := range cfg.Groups {
		if g.ChatID == 0 {
			return nil, fmt.Errorf("groups[%d]: chat_id 不能为空", i)
		}
		if g.Database.Host == "" || g.Database.DBName == "" {
			return nil, fmt.Errorf("groups[%d]: 数据库配置不完整", i)
		}
		if g.Database.Port == 0 {
			cfg.Groups[i].Database.Port = 3306
		}
		if g.VerifyTimeout <= 0 {
			cfg.Groups[i].VerifyTimeout = 300
		}
	}

	return &cfg, nil
}
