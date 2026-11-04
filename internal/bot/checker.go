package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/atomicfile"
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

	// absent: chatID -> (userID -> 确认不在群内的时间)
	// 失效用户大多早已不在群内，记录后在 absentTTL 内跳过，避免每轮都调用 getChatMember
	absentMu sync.Mutex
	absent   map[int64]map[int64]time.Time
	// absentPath 不在群内记录的持久化文件，为空则不持久化
	absentPath string

	running atomic.Bool
}

// absentTTL 不在群内的记录有效期；入群/发言会提前清除，到期后巡检再复查一次
const absentTTL = 6 * time.Hour

// MarkPresent 用户入群或在群内发言时调用，清除其"不在群内"记录
func (c *Checker) MarkPresent(chatID, userID int64) {
	c.absentMu.Lock()
	defer c.absentMu.Unlock()
	if m, ok := c.absent[chatID]; ok {
		delete(m, userID)
	}
}

func (c *Checker) markAbsent(chatID, userID int64) {
	c.absentMu.Lock()
	defer c.absentMu.Unlock()
	m, ok := c.absent[chatID]
	if !ok {
		m = make(map[int64]time.Time)
		c.absent[chatID] = m
	}
	m[userID] = time.Now()
}

func (c *Checker) isAbsent(chatID, userID int64) bool {
	c.absentMu.Lock()
	defer c.absentMu.Unlock()
	at, ok := c.absent[chatID][userID]
	if !ok {
		return false
	}
	if time.Since(at) > absentTTL {
		delete(c.absent[chatID], userID)
		return false
	}
	return true
}

// loadAbsent 从磁盘恢复不在群内的记录，过期条目丢弃
func (c *Checker) loadAbsent() {
	if c.absentPath == "" {
		return
	}
	raw, err := os.ReadFile(c.absentPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("读取不在群内记录失败", "path", c.absentPath, "error", err)
		}
		return
	}
	var data map[int64]map[int64]time.Time
	if err := json.Unmarshal(raw, &data); err != nil {
		slog.Error("解析不在群内记录失败", "path", c.absentPath, "error", err)
		return
	}
	loaded := 0
	for chatID, users := range data {
		for userID, at := range users {
			if time.Since(at) > absentTTL {
				continue
			}
			if c.absent[chatID] == nil {
				c.absent[chatID] = make(map[int64]time.Time)
			}
			c.absent[chatID][userID] = at
			loaded++
		}
	}
	slog.Info("已恢复不在群内记录", "count", loaded)
}

// saveAbsent 把未过期的不在群内记录写入磁盘
func (c *Checker) saveAbsent() {
	if c.absentPath == "" {
		return
	}
	c.absentMu.Lock()
	data := make(map[int64]map[int64]time.Time, len(c.absent))
	for chatID, users := range c.absent {
		m := make(map[int64]time.Time, len(users))
		for userID, at := range users {
			if time.Since(at) > absentTTL {
				delete(users, userID)
				continue
			}
			m[userID] = at
		}
		data[chatID] = m
	}
	c.absentMu.Unlock()

	if err := atomicfile.WriteJSON(c.absentPath, data); err != nil {
		slog.Error("保存不在群内记录失败", "path", c.absentPath, "error", err)
	}
}

// NewChecker 构造一个 Checker，invalidate 可为 nil；absentPath 为不在群内记录的持久化文件路径
func NewChecker(b *tele.Bot, cfg *config.Config, dbClients map[int64]*db.Client, bindings *binding.Store, invalidate Invalidator, absentPath string) *Checker {
	groups := make(map[int64]*config.GroupConfig)
	for i := range cfg.Groups {
		groups[cfg.Groups[i].ChatID] = &cfg.Groups[i]
	}
	c := &Checker{
		bot:        b,
		config:     cfg,
		dbClients:  dbClients,
		groups:     groups,
		bindings:   bindings,
		invalidate: invalidate,
		absent:     make(map[int64]map[int64]time.Time),
		absentPath: absentPath,
	}
	c.loadAbsent()
	return c
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

// TriggerCheck 在后台立即执行一次巡检，已有巡检在进行时返回 false
func (c *Checker) TriggerCheck() bool {
	if c.running.Load() {
		return false
	}
	go c.RunCheck()
	return true
}

// RunCheck 执行一次巡检；同一时间只允许一轮，已有巡检在进行时直接返回
func (c *Checker) RunCheck() {
	if !c.running.CompareAndSwap(false, true) {
		slog.Info("已有巡检正在进行，本次跳过")
		return
	}
	defer c.running.Store(false)
	defer logPanic("巡检")
	defer c.saveAbsent()

	slog.Info("开始巡检...")
	start := time.Now()
	var total checkStats

	for chatID, client := range c.dbClients {
		group := c.groups[chatID]
		s := c.checkGroup(chatID, client, group)
		total.invalid += s.invalid
		total.skippedAbsent += s.skippedAbsent
		total.apiCalls += s.apiCalls
		total.apiErrors += s.apiErrors
		total.notFound += s.notFound
		total.kicked += s.kicked
	}

	slog.Info("巡检完成",
		"total_kicked", total.kicked,
		"invalid_users", total.invalid,
		"skipped_absent", total.skippedAbsent,
		"api_calls", total.apiCalls,
		"api_errors", total.apiErrors,
		"not_found", total.notFound,
		"duration", time.Since(start).Round(time.Second),
	)
}

// checkStats 单轮巡检统计
type checkStats struct {
	invalid       int // 套餐失效的绑定用户数
	skippedAbsent int // 因已确认不在群内而跳过的数量
	apiCalls      int // getChatMember 调用次数
	apiErrors     int // getChatMember 失败次数（不含用户不存在）
	notFound      int // Telegram 判定用户不存在（已注销等），按不在群内处理
	kicked        int
}

// isUserNotFound 判断 getChatMember 错误是否表示该用户在 Telegram 侧不存在
func isUserNotFound(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "PARTICIPANT_ID_INVALID") ||
		strings.Contains(msg, "USER_ID_INVALID") ||
		strings.Contains(msg, "user not found")
}

// invalidEntry 记录一个待踢出用户的失效原因
type invalidEntry struct {
	email  string
	reason string
}

func (c *Checker) checkGroup(chatID int64, client *db.Client, group *config.GroupConfig) checkStats {
	var stats checkStats
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

	stats.invalid = len(expiredUsers)
	if len(expiredUsers) == 0 {
		slog.Debug("无过期用户", "chat_id", chatID)
		return stats
	}

	chat := &tele.Chat{ID: chatID}
	var kickedLines []string
	var lastAPIErr error

	for tgID, entry := range expiredUsers {
		if group.IsExempt(tgID) || c.config.IsAdmin(tgID) {
			continue
		}
		if c.isAbsent(chatID, tgID) {
			stats.skippedAbsent++
			continue
		}

		tgUser := &tele.User{ID: tgID}
		stats.apiCalls++
		member, err := c.bot.ChatMemberOf(chat, tgUser)
		if err != nil && isUserNotFound(err) {
			c.markAbsent(chatID, tgID)
			stats.notFound++
			continue
		}
		if err != nil {
			stats.apiErrors++
			lastAPIErr = err
			var flood tele.FloodError
			if errors.As(err, &flood) && flood.RetryAfter > 0 {
				slog.Warn("巡检触发 Telegram 限流，暂停后继续",
					"chat_id", chatID, "retry_after", flood.RetryAfter,
				)
				time.Sleep(time.Duration(flood.RetryAfter) * time.Second)
			}
			continue
		}

		if member.Role == tele.Left || member.Role == tele.Kicked {
			c.markAbsent(chatID, tgID)
			if c.invalidate != nil {
				c.invalidate(chatID, tgID)
			}
			continue
		}
		// Telegram 不允许踢出群主和管理员
		if member.Role == tele.Creator || member.Role == tele.Administrator {
			slog.Debug("群管理员，跳过巡检踢出", "user_id", tgID)
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
		c.markAbsent(chatID, tgID)
		stats.kicked++
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

	if lastAPIErr != nil {
		slog.Warn("巡检查询群成员存在失败",
			"chat_id", chatID, "failed", stats.apiErrors, "last_error", lastAPIErr,
		)
	}
	return stats
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
