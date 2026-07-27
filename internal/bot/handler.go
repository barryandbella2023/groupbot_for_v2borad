package bot

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/binding"
	"v2board-tg-bot/internal/config"
	"v2board-tg-bot/internal/db"
)

// PendingVerify 待验证的新成员信息
type PendingVerify struct {
	ChatID      int64
	UserID      int64
	MessageID   int
	JoinTime    time.Time
	DisplayName string
}

// Handler 处理所有 Telegram 事件与命令
type Handler struct {
	bot       *tele.Bot
	config    *config.Config
	dbClients map[int64]*db.Client
	groups    map[int64]*config.GroupConfig
	bindings  *binding.Store

	mu      sync.RWMutex
	pending map[int64]*PendingVerify // telegram_id -> 待验证信息

	// verifyCache: chatID -> LRU(userID -> struct{})
	// 已通过验证的用户白名单缓存，避免每条群消息都查 DB
	verifyCache map[int64]*lru.Cache[int64, struct{}]
}

// NewHandler 构造一个 Handler
func NewHandler(b *tele.Bot, cfg *config.Config, dbClients map[int64]*db.Client, bindings *binding.Store) *Handler {
	groups := make(map[int64]*config.GroupConfig)
	verifyCache := make(map[int64]*lru.Cache[int64, struct{}])
	for i := range cfg.Groups {
		g := &cfg.Groups[i]
		groups[g.ChatID] = g
		c, err := lru.New[int64, struct{}](cfg.CacheSize)
		if err != nil {
			// LRU 创建失败时降级为最小容量，保证不 panic
			c, _ = lru.New[int64, struct{}](128)
		}
		verifyCache[g.ChatID] = c
	}
	return &Handler{
		bot:         b,
		config:      cfg,
		dbClients:   dbClients,
		groups:      groups,
		bindings:    bindings,
		pending:     make(map[int64]*PendingVerify),
		verifyCache: verifyCache,
	}
}

// Register 注册所有命令、事件处理器与中间件
func (h *Handler) Register() {
	// 全局中间件：拦截群消息做"首次发言验证"
	h.bot.Use(h.groupVerifyMiddleware())

	h.bot.Handle(tele.OnUserJoined, h.onUserJoined)
	h.bot.Handle(tele.OnChatMember, h.onChatMemberUpdate)
	h.bot.Handle("/start", h.onStart)
	h.bot.Handle("/bind", h.onBind)
	h.bot.Handle("/forcebind", h.onForceBind)
	h.bot.Handle("/unbind", h.onUnbind)
	h.bot.Handle("/status", h.onStatus)
	h.bot.Handle("/check", h.onCheck)
	h.bot.Handle("/cha", h.onCha)

	// 给所有可能在群里出现的消息事件注册 noop，
	// 让中间件能拿到事件做验证；命令则走各自 handler。
	noop := func(c tele.Context) error { return nil }
	for _, ev := range []string{
		tele.OnText, tele.OnPhoto, tele.OnVideo, tele.OnAnimation,
		tele.OnAudio, tele.OnVoice, tele.OnVideoNote, tele.OnDocument,
		tele.OnSticker, tele.OnContact, tele.OnLocation, tele.OnVenue,
		tele.OnDice, tele.OnPoll, tele.OnMedia,
	} {
		h.bot.Handle(ev, noop)
	}
}

// isGroupChat 判断当前是否在群组中
func isGroupChat(c tele.Context) bool {
	return c.Chat() != nil &&
		(c.Chat().Type == tele.ChatGroup || c.Chat().Type == tele.ChatSuperGroup)
}

// rejectInGroup 群内触发非 /status 命令时：删除命令消息，发临时提示
func (h *Handler) rejectInGroup(c tele.Context) error {
	_ = h.bot.Delete(c.Message())
	tip, _ := h.bot.Send(c.Chat(), "💡 该命令仅限私聊使用，请私聊我执行。")
	if tip != nil {
		go func() {
			time.Sleep(5 * time.Second)
			_ = h.bot.Delete(tip)
		}()
	}
	return nil
}

// Invalidate 让缓存中的 verified 状态失效（巡检踢人时调用）
func (h *Handler) Invalidate(chatID, userID int64) {
	if c, ok := h.verifyCache[chatID]; ok {
		c.Remove(userID)
	}
}

// markVerified 把用户加入指定群的"已验证"缓存
func (h *Handler) markVerified(chatID, userID int64) {
	if c, ok := h.verifyCache[chatID]; ok {
		c.Add(userID, struct{}{})
	}
}

// isCachedVerified 命中说明该用户最近已被验证有效，可直接放行
func (h *Handler) isCachedVerified(chatID, userID int64) bool {
	c, ok := h.verifyCache[chatID]
	if !ok {
		return false
	}
	_, hit := c.Get(userID)
	return hit
}

// groupVerifyMiddleware 群消息中间件：保证发言者是已验证用户，否则启动验证流程
func (h *Handler) groupVerifyMiddleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			chat := c.Chat()
			if chat == nil || (chat.Type != tele.ChatGroup && chat.Type != tele.ChatSuperGroup) {
				return next(c)
			}
			group, ok := h.groups[chat.ID]
			if !ok {
				return next(c)
			}
			user := c.Sender()
			if user == nil || user.IsBot {
				return next(c)
			}
			msg := c.Message()
			// service messages（加群/退群/置顶等）不走验证逻辑
			if msg != nil && (msg.UserJoined != nil || msg.UserLeft != nil ||
				msg.NewGroupTitle != "" || msg.NewGroupPhoto != nil ||
				msg.GroupPhotoDeleted || msg.PinnedMessage != nil ||
				msg.GroupCreated || msg.MigrateTo != 0 || msg.MigrateFrom != 0) {
				return next(c)
			}

			slog.Debug("收到群消息",
				"chat_id", chat.ID, "user_id", user.ID,
				"username", user.Username, "text_len", len(c.Text()),
			)

			if h.ensureVerified(chat, user, group, msg) {
				return next(c)
			}
			// 未通过：消息已删除 / 验证流程已启动，本次事件不再继续
			return nil
		}
	}
}

// ensureVerified 检查发言者是否已通过验证，未通过则启动验证流程并返回 false
func (h *Handler) ensureVerified(chat *tele.Chat, user *tele.User, group *config.GroupConfig, msg *tele.Message) bool {
	chatID := chat.ID
	userID := user.ID

	if group.IsExempt(userID) {
		h.markVerified(chatID, userID)
		return true
	}

	if h.isCachedVerified(chatID, userID) {
		return true
	}

	// 已在 pending 中说明刚加群正在验证窗口里；删消息但不重复触发
	h.mu.RLock()
	_, inPending := h.pending[userID]
	h.mu.RUnlock()
	if inPending {
		if msg != nil {
			_ = h.bot.Delete(msg)
		}
		return false
	}

	client, ok := h.dbClients[chatID]
	if ok {
		if b, bound := h.bindings.Get(userID); bound && b.DBName == group.Database.DBName {
			if u, err := client.FindUserByEmail(b.Email); err == nil && u != nil && db.IsUserValid(u) {
				h.markVerified(chatID, userID)
				return true
			}
		}
	}

	slog.Info("未验证用户在群组发言，启动验证流程",
		"chat_id", chatID, "user_id", userID, "username", user.Username,
	)
	if msg != nil {
		_ = h.bot.Delete(msg)
	}
	h.startVerify(chat, user, group, false)
	return false
}

// startVerify 对指定用户启动验证流程：禁言 + 弹按钮 + 加 pending + 启动 timeout
// isNewJoin=true 表示由入群事件触发，文案带"欢迎"
func (h *Handler) startVerify(chat *tele.Chat, user *tele.User, group *config.GroupConfig, isNewJoin bool) {
	userID := user.ID

	// 占位防并发重复触发
	h.mu.Lock()
	if _, exists := h.pending[userID]; exists {
		h.mu.Unlock()
		return
	}
	h.pending[userID] = &PendingVerify{
		ChatID:      chat.ID,
		UserID:      userID,
		JoinTime:    time.Now(),
		DisplayName: displayName(user),
	}
	h.mu.Unlock()

	if err := h.bot.Restrict(chat, &tele.ChatMember{
		User:   user,
		Rights: tele.Rights{CanSendMessages: false},
	}); err != nil {
		slog.Error("禁言用户失败", "user_id", userID, "error", err)
	}

	botUsername := h.bot.Me.Username
	verifyURL := fmt.Sprintf("https://t.me/%s?start=verify_%d", botUsername, chat.ID)

	btn := &tele.ReplyMarkup{}
	btnVerify := btn.URL("👉 点击验证", verifyURL)
	btn.Inline(btn.Row(btnVerify))

	var prompt string
	safeName := mdEscape(displayName(user))
	if isNewJoin {
		prompt = fmt.Sprintf("👋 欢迎 [%s](tg://user?id=%d)！\n\n"+
			"请在 *%d 秒*内点击下方按钮完成验证，否则将被移出群组。",
			safeName, userID, group.VerifyTimeout)
	} else {
		prompt = fmt.Sprintf("⚠️ [%s](tg://user?id=%d) 检测到您尚未通过验证。\n\n"+
			"请在 *%d 秒*内点击下方按钮完成验证，否则将被移出群组。",
			safeName, userID, group.VerifyTimeout)
	}

	msg, err := h.bot.Send(chat, prompt, tele.ModeMarkdown, btn)
	if err != nil {
		slog.Error("发送验证消息失败", "error", err)
		// 发送失败也保留 pending，timeout 仍会处理
	} else {
		h.mu.Lock()
		if pv, ok := h.pending[userID]; ok {
			pv.MessageID = msg.ID
		}
		h.mu.Unlock()
	}

	go h.verifyTimeout(userID, group.VerifyTimeout)
}

// onChatMemberUpdate 通过 chat_member 更新检测用户加入（兜底 OnUserJoined 失效的情况）
func (h *Handler) onChatMemberUpdate(c tele.Context) error {
	cm := c.ChatMember()
	if cm == nil || cm.NewChatMember == nil || cm.OldChatMember == nil {
		return nil
	}

	newRole := cm.NewChatMember.Role
	oldRole := cm.OldChatMember.Role
	user := cm.NewChatMember.User
	chat := cm.Chat

	if user == nil || chat == nil || user.IsBot {
		return nil
	}

	// 只处理"从非成员变为成员"的情况
	isJoin := (oldRole == tele.Left || oldRole == tele.Kicked) &&
		(newRole == tele.Member || newRole == tele.Administrator || newRole == tele.Creator)
	if !isJoin {
		return nil
	}

	slog.Info("检测到用户加入(chat_member)",
		"chat_id", chat.ID, "user_id", user.ID,
		"username", user.Username, "old_role", oldRole, "new_role", newRole,
	)

	group, ok := h.groups[chat.ID]
	if !ok {
		return nil
	}

	if group.IsExempt(user.ID) {
		slog.Info("白名单用户，跳过验证", "user_id", user.ID)
		h.markVerified(chat.ID, user.ID)
		_, _ = h.bot.Send(chat,
			fmt.Sprintf("👋 欢迎 [%s](tg://user?id=%d) 加入！", mdEscape(displayName(user)), user.ID),
			tele.ModeMarkdown,
		)
		return nil
	}

	h.startVerify(chat, user, group, true)
	return nil
}

// onUserJoined 用户加入群组时触发（service message 方式，部分群组可能不触发）
func (h *Handler) onUserJoined(c tele.Context) error {
	chatID := c.Chat().ID
	user := c.Sender()
	if user == nil || user.IsBot {
		return nil
	}
	userID := user.ID

	slog.Info("新用户加入群组",
		"chat_id", chatID,
		"user_id", userID,
		"username", user.Username,
	)

	group, ok := h.groups[chatID]
	if !ok {
		return nil
	}

	if group.IsExempt(userID) {
		slog.Info("白名单用户，跳过验证", "user_id", userID)
		h.markVerified(chatID, userID)
		_, _ = c.Bot().Send(c.Chat(),
			fmt.Sprintf("👋 欢迎 [%s](tg://user?id=%d) 加入！", mdEscape(displayName(user)), userID),
			tele.ModeMarkdown,
		)
		return nil
	}

	h.startVerify(c.Chat(), user, group, true)
	return nil
}

// verifyTimeout 超时未验证则踢出
func (h *Handler) verifyTimeout(userID int64, timeoutSec int) {
	time.Sleep(time.Duration(timeoutSec) * time.Second)

	h.mu.Lock()
	pv, exists := h.pending[userID]
	if !exists {
		h.mu.Unlock()
		return
	}
	delete(h.pending, userID)
	h.mu.Unlock()

	chat := &tele.Chat{ID: pv.ChatID}
	user := &tele.User{ID: userID}

	member, mErr := h.bot.ChatMemberOf(chat, user)
	if mErr == nil && member.Role == tele.Kicked {
		slog.Info("用户已被其他来源封禁，跳过踢出操作", "user_id", userID)
		return
	}

	err := h.bot.Ban(chat, &tele.ChatMember{
		User:            user,
		RestrictedUntil: time.Now().Add(60 * time.Second).Unix(),
	})
	if err != nil {
		slog.Error("超时踢出失败", "user_id", userID, "error", err)
		return
	}
	time.Sleep(500 * time.Millisecond)
	if err := h.bot.Unban(chat, user); err != nil {
		slog.Warn("Unban失败，重试一次", "user_id", userID, "error", err)
		time.Sleep(1 * time.Second)
		if err := h.bot.Unban(chat, user); err != nil {
			slog.Error("Unban再次失败，将在60秒后自动解除", "user_id", userID, "error", err)
		}
	}

	h.Invalidate(pv.ChatID, userID)
	slog.Info("用户验证超时，已踢出", "user_id", userID, "chat_id", pv.ChatID)

	if pv.MessageID != 0 {
		_ = h.bot.Delete(&tele.Message{ID: pv.MessageID, Chat: chat})
	}

	kickText := fmt.Sprintf("⏰ [%s](tg://user?id=%d) 验证超时，已被移出群组。", mdEscape(pv.DisplayName), userID)
	kickMsg, _ := h.bot.Send(chat, kickText, tele.ModeMarkdown)
	if kickMsg != nil {
		go func() {
			time.Sleep(10 * time.Second)
			_ = h.bot.Delete(kickMsg)
		}()
	}
}

// onStart 处理 /start 命令，区分普通启动和验证跳转
func (h *Handler) onStart(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	payload := c.Message().Payload
	if strings.HasPrefix(payload, "verify_") {
		return h.handleVerifyStart(c, payload)
	}
	return c.Send(h.buildStartMessage(), tele.ModeMarkdown)
}

// buildStartMessage 基于 config 中的 profile 构建 /start 响应
func (h *Handler) buildStartMessage() string {
	var sb strings.Builder

	p := h.config.Telegram.Profile
	if p.Description != "" {
		sb.WriteString(p.Description)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("👋 欢迎使用群组管理机器人\n\n")
		sb.WriteString("本机器人用于管理群组成员，只有拥有有效套餐的用户才能加入。\n\n")
	}

	if len(p.Commands) > 0 {
		sb.WriteString("📝 可用命令：\n")
		for _, cmd := range p.Commands {
			sb.WriteString(fmt.Sprintf("`/%s` - %s\n", cmd.Command, cmd.Description))
		}
	} else {
		sb.WriteString("📝 可用命令：\n")
		sb.WriteString("`/bind 邮箱 密码` - 绑定面板账户\n")
		sb.WriteString("`/unbind` - 解除绑定\n")
		sb.WriteString("`/status` - 查看套餐状态\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}

// handleVerifyStart 用户点击验证按钮跳转过来
func (h *Handler) handleVerifyStart(c tele.Context, _ string) error {
	userID := c.Sender().ID

	h.mu.RLock()
	pv, hasPending := h.pending[userID]
	h.mu.RUnlock()

	if !hasPending {
		return c.Send("✅ 您已通过验证，无需重复操作。")
	}

	client, ok := h.dbClients[pv.ChatID]
	if !ok {
		return c.Send("❌ 系统错误，请联系管理员。")
	}

	b, bound := h.bindings.Get(userID)
	if !bound {
		slog.Info("用户未绑定，提示绑定", "telegram_id", userID)
		return c.Send(
			"📝 请先绑定您的面板账户：\n\n"+
				"发送：`/bind 您的邮箱 您的密码`\n\n"+
				"示例：`/bind user@example.com mypassword`\n\n"+
				"绑定成功后将自动完成验证并解除禁言。",
			tele.ModeMarkdown,
		)
	}

	user, err := client.FindUserByEmail(b.Email)
	if err != nil || user == nil {
		slog.Warn("本地绑定的邮箱在数据库中未找到",
			"telegram_id", userID, "email", b.Email,
		)
		return c.Send("❌ 未找到您的账户信息，请重新绑定：`/bind 邮箱 密码`", tele.ModeMarkdown)
	}

	slog.Info("通过本地绑定找到用户",
		"telegram_id", userID, "email", user.Email,
		"plan_id", user.PlanID, "expired_at", user.ExpiredAt,
		"banned", user.Banned, "valid", db.IsUserValid(user),
	)

	if !db.IsUserValid(user) {
		return c.Send(fmt.Sprintf("❌ 验证失败：%s\n\n请前往官网购买/续费套餐。", describeInvalid(user)))
	}

	h.approveUser(userID, planNameOf(client, user))
	return c.Send("✅ 验证通过！已解除禁言，欢迎加入群组！")
}

// approveUser 通过验证：解除禁言 + 清理消息
func (h *Handler) approveUser(userID int64, planName string) {
	h.mu.Lock()
	pv, exists := h.pending[userID]
	if !exists {
		h.mu.Unlock()
		return
	}
	delete(h.pending, userID)
	h.mu.Unlock()

	chat := &tele.Chat{ID: pv.ChatID}
	user := &tele.User{ID: userID}

	err := h.bot.Restrict(chat, &tele.ChatMember{
		User: user,
		Rights: tele.Rights{
			CanSendMessages: true,
			CanSendMedia:    true,
			CanSendOther:    true,
			CanAddPreviews:  true,
		},
	})
	if err != nil {
		slog.Error("解除禁言失败", "user_id", userID, "error", err)
	}

	if pv.MessageID != 0 {
		_ = h.bot.Delete(&tele.Message{ID: pv.MessageID, Chat: chat})
	}

	h.markVerified(pv.ChatID, userID)

	name := fmt.Sprintf("用户%d", userID)
	member, err := h.bot.ChatMemberOf(chat, user)
	if err == nil && member.User != nil {
		name = displayName(member.User)
	}

	safeName := mdEscape(name)
	var welcomeText string
	if planName != "" {
		welcomeText = fmt.Sprintf("👏 欢迎 尊贵的 %s 用户 [%s](tg://user?id=%d)", mdEscape(planName), safeName, userID)
	} else {
		welcomeText = fmt.Sprintf("👏 欢迎 [%s](tg://user?id=%d)", safeName, userID)
	}
	welcomeMsg, _ := h.bot.Send(chat, welcomeText, tele.ModeMarkdown)
	if welcomeMsg != nil {
		go func() {
			time.Sleep(60 * time.Second)
			_ = h.bot.Delete(welcomeMsg)
		}()
	}

	slog.Info("用户验证通过", "user_id", userID, "chat_id", pv.ChatID)
}

// onBind 处理 /bind 命令
func (h *Handler) onBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	args := strings.Fields(c.Message().Text)
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/bind 邮箱 密码`\n\n示例：`/bind user@example.com mypassword`",
			tele.ModeMarkdown,
		)
	}

	email := args[1]
	password := args[2]
	userID := c.Sender().ID

	var foundUser *db.V2User
	var foundDBName string

	for chatID, client := range h.dbClients {
		u, err := client.FindUserByEmail(email)
		if err != nil {
			slog.Error("查询用户失败", "email", email, "error", err)
			continue
		}
		if u != nil {
			foundUser = u
			foundDBName = h.groups[chatID].Database.DBName
			break
		}
	}

	if foundUser == nil {
		return c.Send("❌ 未找到该邮箱对应的账户，请检查邮箱是否正确。")
	}

	algo := ""
	if foundUser.PasswordAlgo.Valid {
		algo = foundUser.PasswordAlgo.String
	}
	salt := ""
	if foundUser.PasswordSalt.Valid {
		salt = foundUser.PasswordSalt.String
	}

	if !db.VerifyPassword(algo, salt, password, foundUser.Password) {
		return c.Send("❌ 密码错误，请重试。")
	}

	if foundUser.Banned != 0 {
		return c.Send("❌ 您的账户已被封禁，无法绑定。")
	}

	if existingTG := h.bindings.FindByEmail(email); existingTG != 0 && existingTG != userID {
		return c.Send("❌ 该邮箱已被其他 Telegram 账号绑定。\n如需更换，请先在原账号执行 /unbind 解绑。")
	}

	if err := h.bindings.Set(userID, email, foundDBName); err != nil {
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return c.Send("❌ 绑定失败，请稍后重试。")
	}

	slog.Info("用户绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)

	h.mu.RLock()
	pv, hasPending := h.pending[userID]
	h.mu.RUnlock()

	if hasPending {
		// 必须用待验证群组对应的数据库来判断，而不是绑定时搜到的数据库
		groupDB, ok := h.dbClients[pv.ChatID]
		if ok {
			groupUser, err := groupDB.FindUserByEmail(email)
			if err == nil && groupUser != nil && db.IsUserValid(groupUser) {
				slog.Info("绑定时自动审批通过",
					"telegram_id", userID, "email", email,
					"plan_id", groupUser.PlanID, "expired_at", groupUser.ExpiredAt,
				)
				h.approveUser(userID, planNameOf(groupDB, groupUser))
				return c.Send(fmt.Sprintf("✅ 绑定成功！\n\n邮箱：%s\n已自动完成验证并解除禁言 🎉", email))
			}
			reason := describeInvalid(groupUser)
			slog.Info("绑定成功但套餐无效，未审批",
				"telegram_id", userID, "email", email, "reason", reason,
			)
			return c.Send(fmt.Sprintf(
				"✅ 账户绑定成功！\n\n但验证未通过：%s\n请前往官网购买/续费套餐。", reason,
			))
		}
	}

	if !db.IsUserValid(foundUser) {
		return c.Send(
			"✅ 账户绑定成功！\n\n" +
				"但您当前没有有效套餐，请前往官网购买套餐后加入群组。",
		)
	}

	return c.Send(fmt.Sprintf("✅ 绑定成功！\n\n邮箱：%s\n现在您可以加入群组了。", email))
}

// onForceBind 强制绑定：通过邮箱+密码验证身份后，解除该邮箱的旧绑定并绑定到当前 TG 号
func (h *Handler) onForceBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}

	args := strings.Fields(c.Message().Text)
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/forcebind 邮箱 密码`\n\n"+
				"用于强制将邮箱绑定到当前 Telegram 账号。\n"+
				"如果该邮箱已被其他 TG 号绑定，验证密码后会自动解除旧绑定。",
			tele.ModeMarkdown,
		)
	}

	email := args[1]
	password := args[2]
	userID := c.Sender().ID

	var foundUser *db.V2User
	var foundDBName string

	for chatID, client := range h.dbClients {
		u, err := client.FindUserByEmail(email)
		if err != nil {
			continue
		}
		if u != nil {
			foundUser = u
			foundDBName = h.groups[chatID].Database.DBName
			break
		}
	}

	if foundUser == nil {
		return c.Send("❌ 未找到该邮箱对应的账户，请检查邮箱是否正确。")
	}

	algo := ""
	if foundUser.PasswordAlgo.Valid {
		algo = foundUser.PasswordAlgo.String
	}
	salt := ""
	if foundUser.PasswordSalt.Valid {
		salt = foundUser.PasswordSalt.String
	}

	if !db.VerifyPassword(algo, salt, password, foundUser.Password) {
		return c.Send("❌ 密码错误，请重试。")
	}

	if foundUser.Banned != 0 {
		return c.Send("❌ 您的账户已被封禁，无法绑定。")
	}

	oldTG := h.bindings.FindByEmail(email)
	if oldTG != 0 && oldTG != userID {
		_ = h.bindings.Delete(oldTG)
		for chatID := range h.verifyCache {
			h.Invalidate(chatID, oldTG)
		}
		slog.Info("强制解除旧绑定", "email", email, "old_tg", oldTG, "new_tg", userID)
	}

	if err := h.bindings.Set(userID, email, foundDBName); err != nil {
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return c.Send("❌ 绑定失败，请稍后重试。")
	}

	slog.Info("用户强制绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)

	// 检查是否有待验证的入群请求
	h.mu.RLock()
	pv, hasPending := h.pending[userID]
	h.mu.RUnlock()

	if hasPending {
		groupDB, ok := h.dbClients[pv.ChatID]
		if ok {
			groupUser, err := groupDB.FindUserByEmail(email)
			if err == nil && groupUser != nil && db.IsUserValid(groupUser) {
				h.approveUser(userID, planNameOf(groupDB, groupUser))
				if oldTG != 0 && oldTG != userID {
					return c.Send(fmt.Sprintf("✅ 已解除旧绑定并重新绑定成功！\n\n邮箱：%s\n已自动完成验证并解除禁言 🎉", email))
				}
				return c.Send(fmt.Sprintf("✅ 绑定成功！\n\n邮箱：%s\n已自动完成验证并解除禁言 🎉", email))
			}
			reason := describeInvalid(groupUser)
			return c.Send(fmt.Sprintf("✅ 绑定成功！\n\n但验证未通过：%s\n请前往官网购买/续费套餐。", reason))
		}
	}

	if oldTG != 0 && oldTG != userID {
		return c.Send(fmt.Sprintf("✅ 已解除旧绑定并重新绑定成功！\n\n邮箱：%s\n现在您可以加入群组了。", email))
	}
	return c.Send(fmt.Sprintf("✅ 绑定成功！\n\n邮箱：%s\n现在您可以加入群组了。", email))
}

// onUnbind 解除绑定
func (h *Handler) onUnbind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	userID := c.Sender().ID
	if _, ok := h.bindings.Get(userID); !ok {
		return c.Send("未找到您的绑定记录。")
	}
	if err := h.bindings.Delete(userID); err != nil {
		return c.Send("❌ 解绑失败，请稍后重试。")
	}
	// 解绑后失效所有群的缓存，下次他发言会被重新校验
	for chatID := range h.verifyCache {
		h.Invalidate(chatID, userID)
	}
	return c.Send("✅ 已解除绑定。")
}

// onStatus 查询套餐状态
func (h *Handler) onStatus(c tele.Context) error {
	sender := c.Sender()
	if sender == nil || sender.IsBot {
		// 匿名管理员发送，sender 是 GroupAnonymousBot，无法查询
		if isGroupChat(c) {
			_ = h.bot.Delete(c.Message())
		}
		return nil
	}
	userID := sender.ID
	b, hasBind := h.bindings.Get(userID)

	inGroup := isGroupChat(c)

	var results []string
	for chatID, client := range h.dbClients {
		group := h.groups[chatID]
		dbName := group.Database.DBName

		if group.IsExempt(userID) {
			results = append(results, "⭐ 白名单用户，免验证")
			continue
		}

		if hasBind && b.DBName == dbName {
			user, err := client.FindUserByEmail(b.Email)
			if err != nil || user == nil {
				results = append(results, "📦 ❓ 查询失败")
				continue
			}
			results = append(results, formatStatusLine(client, user, inGroup))
		}
	}

	if len(results) == 0 {
		return c.Send("未找到您的账户信息，请先使用 `/bind 邮箱 密码` 绑定。", tele.ModeMarkdown)
	}

	text := "━━━━━ 📊 套餐状态 ━━━━━\n\n" + strings.Join(results, "\n\n━━━━━━━━━━━━━━━━━\n\n")

	if inGroup {
		// 群内：删除用户的命令消息，回复 30 秒后自动删除
		_ = h.bot.Delete(c.Message())
		reply, _ := h.bot.Send(c.Chat(), text, tele.ModeMarkdown)
		if reply != nil {
			go func() {
				time.Sleep(30 * time.Second)
				_ = h.bot.Delete(reply)
			}()
		}
		return nil
	}
	return c.Send(text, tele.ModeMarkdown)
}

// onCheck 管理员手动触发巡检（预留，目前仅返回提示）
func (h *Handler) onCheck(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	if !h.config.IsAdmin(c.Sender().ID) {
		return c.Send("❓ 无效命令，请输入 /start 查看帮助。")
	}
	return c.Send("✅ 巡检任务已触发，请稍候查看日志。")
}

// onCha 管理员通过 Telegram ID 查询用户信息
func (h *Handler) onCha(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	if !h.config.IsAdmin(c.Sender().ID) {
		return c.Send("❓ 无效命令，请输入 /start 查看帮助。")
	}

	args := strings.Fields(c.Message().Text)
	if len(args) != 2 {
		return c.Send("📝 用法：`/cha TelegramID`\n\n示例：`/cha 123456789`", tele.ModeMarkdown)
	}

	var tgID int64
	if _, err := fmt.Sscanf(args[1], "%d", &tgID); err != nil {
		return c.Send("❌ 请输入有效的数字 ID")
	}

	b, bound := h.bindings.Get(tgID)
	if !bound {
		return c.Send(fmt.Sprintf("未找到 Telegram ID `%d` 的绑定记录。", tgID), tele.ModeMarkdown)
	}

	for _, client := range h.dbClients {
		user, err := client.FindUserByEmail(b.Email)
		if err != nil || user == nil {
			continue
		}
		return c.Send(
			fmt.Sprintf("🔍 查询结果（TG ID: `%d`）\n\n%s", tgID, formatUserInfo(client, user)),
			tele.ModeMarkdown,
		)
	}

	return c.Send(
		fmt.Sprintf("🔍 查询结果（TG ID: `%d`）\n\n📧 邮箱：`%s`\n⚠️ 数据库中未找到该用户", tgID, b.Email),
		tele.ModeMarkdown,
	)
}
