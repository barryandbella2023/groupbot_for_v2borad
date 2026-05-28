package bot

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/binding"
	"v2board-tg-bot/internal/config"
	"v2board-tg-bot/internal/db"
)

// Invalidator 在踢人后通知外部清理缓存（由 Handler 提供）
type Invalidator func(chatID, userID int64)

// Checker 负责定时巡检，踢出过期/封禁的用户
type Checker struct {
	bot         *tele.Bot
	config      *config.Config
	dbClients   map[int64]*db.Client
	groups      map[int64]*config.GroupConfig
	bindings    *binding.Store
	invalidate  Invalidator
}

// NewChecker 构造一个 Checker，invalidate 可为 nil
func NewChecker(b *tele.Bot, cfg *config.Config, dbClients map[int64]*db.Client, bindings *binding.Store, invalidate Invalidator) *Checker {
	groups := make(map[int64]*config.GroupConfig)
	for i := range cfg.Groups {
		groups[cfg.Groups[i].ChatID] = &cfg.Groups[i]
	}
	return &Checker{
		bot:        b,
		config:     cfg,
		dbClients:  dbClients,
		groups:     groups,
		bindings:   bindings,
		invalidate: invalidate,
	}
}

// Start 启动定时巡检，阻塞运行
func (c *Checker) Start() {
	interval := time.Duration(c.config.CheckInterval) * time.Second
	slog.Info("定时巡检已启动", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		c.RunCheck()
	}
}

// RunCheck 执行一次巡检
func (c *Checker) RunCheck() {
	slog.Info("开始巡检...")
	totalKicked := 0

	for chatID, client := range c.dbClients {
		group := c.groups[chatID]
		totalKicked += c.checkGroup(chatID, client, group)
	}

	slog.Info("巡检完成", "total_kicked", totalKicked)
}

// invalidEntry 记录一个待踢出用户的失效原因
type invalidEntry struct {
	email  string
	reason string
}

func (c *Checker) checkGroup(chatID int64, client *db.Client, group *config.GroupConfig) int {
	expiredUsers := make(map[int64]invalidEntry)

	// 巡检以本地 bindings.json 为权威源（DB 中 telegram_id 字段在启动时已种子导入）
	// 遍历本群对应库下所有本地绑定，按 email 查 DB 套餐状态
	dbName := group.Database.DBName
	boundUsers := c.bindings.GetAllForDB(dbName)
	for tgID, email := range boundUsers {
		user, err := client.FindUserByEmail(email)
		if err != nil {
			slog.Error("查询用户失败", "telegram_id", tgID, "email", email, "error", err)
			continue
		}
		if user == nil || !db.IsUserValid(user) {
			expiredUsers[tgID] = invalidEntry{email: email, reason: describeInvalid(user)}
		}
	}

	if len(expiredUsers) == 0 {
		slog.Debug("无过期用户", "chat_id", chatID)
		return 0
	}

	chat := &tele.Chat{ID: chatID}
	kicked := 0
	var kickedLines []string

	for tgID, entry := range expiredUsers {
		if group.IsExempt(tgID) {
			continue
		}

		tgUser := &tele.User{ID: tgID}
		member, err := c.bot.ChatMemberOf(chat, tgUser)
		if err != nil {
			continue
		}

		if member.Role == tele.Left {
			continue
		}
		if member.Role == tele.Kicked {
			slog.Debug("用户已被其他来源封禁，跳过", "user_id", tgID)
			continue
		}

		name := fmt.Sprintf("用户%d", tgID)
		if member.User != nil {
			name = displayName(member.User)
		}

		err = c.bot.Ban(chat, &tele.ChatMember{
			User:            tgUser,
			RestrictedUntil: time.Now().Add(60 * time.Second).Unix(),
		})
		if err != nil {
			slog.Error("踢出用户失败", "user_id", tgID, "email", entry.email, "error", err)
			continue
		}

		time.Sleep(500 * time.Millisecond)
		if err := c.bot.Unban(chat, tgUser); err != nil {
			slog.Warn("Unban失败，重试一次", "user_id", tgID, "error", err)
			time.Sleep(1 * time.Second)
			if err := c.bot.Unban(chat, tgUser); err != nil {
				slog.Error("Unban再次失败，将在60秒后自动解除", "user_id", tgID, "error", err)
			}
		}

		if c.invalidate != nil {
			c.invalidate(chatID, tgID)
		}
		kicked++
		kickedLines = append(kickedLines, fmt.Sprintf("• %s 因%s被移除", name, entry.reason))
		slog.Info("已踢出失效用户",
			"chat_id", chatID, "user_id", tgID, "email", entry.email, "reason", entry.reason,
		)

		_, _ = c.bot.Send(tgUser,
			fmt.Sprintf("⚠️ 您已被移出群组\n原因：%s\n\n请前往官网处理后重新申请加入。", entry.reason),
		)

		time.Sleep(500 * time.Millisecond)
	}

	// 在群组中发送踢出通知，合并为一条消息，注意 TG 4096 字符限制
	// 群内通知 10 秒后自动删除，避免长期堆积
	if len(kickedLines) > 0 {
		const maxLen = 4096 - 10
		var batch []string
		currentLen := 0

		for _, line := range kickedLines {
			lineLen := len([]rune(line)) + 1
			if currentLen+lineLen > maxLen && len(batch) > 0 {
				c.sendAutoDelete(chat, strings.Join(batch, "\n"), 10*time.Second)
				batch = batch[:0]
				currentLen = 0
				time.Sleep(500 * time.Millisecond)
			}
			batch = append(batch, line)
			currentLen += lineLen
		}
		if len(batch) > 0 {
			c.sendAutoDelete(chat, strings.Join(batch, "\n"), 10*time.Second)
		}
	}

	return kicked
}

// sendAutoDelete 在群组发送消息，并在指定时间后自动删除
func (c *Checker) sendAutoDelete(chat *tele.Chat, text string, ttl time.Duration) {
	msg, err := c.bot.Send(chat, text)
	if err != nil || msg == nil {
		return
	}
	go func() {
		time.Sleep(ttl)
		if err := c.bot.Delete(msg); err != nil {
			slog.Debug("删除巡检通知失败", "chat_id", chat.ID, "msg_id", msg.ID, "error", err)
		}
	}()
}
