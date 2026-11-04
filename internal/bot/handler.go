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

// passwordCheckConcurrency 同时进行的密码校验上限，bcrypt 较耗 CPU
const passwordCheckConcurrency = 2

// PendingVerify 待验证的新成员信息
type PendingVerify struct {
	ChatID      int64     `json:"chat_id"`
	UserID      int64     `json:"user_id"`
	MessageID   int       `json:"message_id"`
	JoinTime    time.Time `json:"join_time"`
	Deadline    time.Time `json:"deadline"`
	DisplayName string    `json:"display_name"`
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

	// pendingPath 待验证列表的持久化文件，为空则不持久化
	pendingPath string
	stateMu     sync.Mutex

	// verifyCache: chatID -> LRU(userID -> struct{})
	// 已通过验证的用户白名单缓存，避免每条群消息都查 DB
	verifyCache map[int64]*lru.Cache[int64, struct{}]

	bindLimit   *bindLimiter
	privateGate *rateGate
	timeouts    *windowCounter
	joinDedup   *joinDedup
	pwSem       chan struct{}

	// onPresent 在确认用户位于群内（入群/发言）时回调，可为 nil
	onPresent func(chatID, userID int64)
	// triggerCheck 立即执行一次巡检，返回 false 表示已有巡检在进行，可为 nil
	triggerCheck func() bool
}

// OnPresent 设置用户在群内时的回调，须在 bot 启动前调用
func (h *Handler) OnPresent(fn func(chatID, userID int64)) {
	h.onPresent = fn
}

// OnCheckTrigger 设置 /check 命令触发巡检的方法，须在 bot 启动前调用
func (h *Handler) OnCheckTrigger(fn func() bool) {
	h.triggerCheck = fn
}

func (h *Handler) notifyPresent(chatID, userID int64) {
	if h.onPresent != nil {
		h.onPresent(chatID, userID)
	}
}

// NewHandler 构造一个 Handler；pendingPath 为待验证列表的持久化文件路径
func NewHandler(b *tele.Bot, cfg *config.Config, dbClients map[int64]*db.Client, bindings *binding.Store, pendingPath string) *Handler {
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
		pendingPath: pendingPath,
		verifyCache: verifyCache,
		bindLimit:   newBindLimiter(),
		privateGate: newRateGate(privateMinInterval),
		timeouts:    newWindowCounter(timeoutWindow),
		joinDedup:   newJoinDedup(),
		pwSem:       make(chan struct{}, passwordCheckConcurrency),
	}
}

// Register 注册所有命令、事件处理器与中间件
func (h *Handler) Register() {
	h.bot.Use(recoverMiddleware())
	h.bot.Use(h.privateRateMiddleware())
	// 拦截群消息做"首次发言验证"
	h.bot.Use(h.groupVerifyMiddleware())

	h.bot.Handle(tele.OnUserJoined, h.onUserJoined)
	h.bot.Handle(tele.OnChatMember, h.onChatMemberUpdate)
	h.bot.Handle(tele.OnChatJoinRequest, h.onJoinRequest)
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

// privateRateMiddleware 私聊消息限速：同一用户间隔过短的消息直接丢弃，管理员不限
func (h *Handler) privateRateMiddleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			chat, sender := c.Chat(), c.Sender()
			if chat == nil || chat.Type != tele.ChatPrivate || sender == nil || h.config.IsAdmin(sender.ID) {
				return next(c)
			}
			if !h.privateGate.allow(sender.ID) {
				slog.Debug("私聊消息过于频繁，已丢弃", "user_id", sender.ID)
				return nil
			}
			return next(c)
		}
	}
}

// groupVerifyMiddleware 群消息中间件：保证发言者是已验证用户，否则启动验证流程
// 只处理群内的普通消息；入群事件、入群申请等非消息更新直接交给各自的处理器
func (h *Handler) groupVerifyMiddleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			msg := c.Message()
			chat := c.Chat()
			if msg == nil || chat == nil || (chat.Type != tele.ChatGroup && chat.Type != tele.ChatSuperGroup) {
				return next(c)
			}
			group, ok := h.groups[chat.ID]
			if !ok {
				return next(c)
			}

			// 以频道身份发送的消息，发送者是 Channel_Bot，必须在 IsBot 判断之前处理
			if msg.SenderChat != nil {
				if h.allowSenderChat(chat, group, msg) {
					return next(c)
				}
				_ = h.bot.Delete(msg)
				slog.Info("已删除以频道身份发送的消息",
					"chat_id", chat.ID, "sender_chat_id", msg.SenderChat.ID, "sender_chat_title", msg.SenderChat.Title,
				)
				return nil
			}

			user := c.Sender()
			if user == nil || user.IsBot {
				return next(c)
			}
			// service messages（加群/退群/置顶等）不走验证逻辑
			if msg.UserJoined != nil || msg.UsersJoined != nil || msg.UserLeft != nil ||
				msg.NewGroupTitle != "" || msg.NewGroupPhoto != nil ||
				msg.GroupPhotoDeleted || msg.PinnedMessage != nil ||
				msg.GroupCreated || msg.MigrateTo != 0 || msg.MigrateFrom != 0 {
				return next(c)
			}
			h.notifyPresent(chat.ID, user.ID)

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

// allowSenderChat 以频道/群身份发送的消息中，只放行：匿名管理员、关联频道自动转发、白名单中的频道
func (h *Handler) allowSenderChat(chat *tele.Chat, group *config.GroupConfig, msg *tele.Message) bool {
	return msg.SenderChat.ID == chat.ID || msg.AutomaticForward || group.IsExempt(msg.SenderChat.ID)
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

	if _, u, bound, err := h.lookupBoundUser(chatID, userID); err == nil && bound && db.IsUserValid(u) {
		h.markVerified(chatID, userID)
		return true
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
	timeout := time.Duration(group.VerifyTimeout) * time.Second

	// 占位防并发重复触发
	h.mu.Lock()
	if _, exists := h.pending[userID]; exists {
		h.mu.Unlock()
		return
	}
	now := time.Now()
	h.pending[userID] = &PendingVerify{
		ChatID:      chat.ID,
		UserID:      userID,
		JoinTime:    now,
		Deadline:    now.Add(timeout),
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
	h.savePending()

	go h.verifyTimeout(userID, timeout)
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
	h.handleJoin(chat, user, group)
	return nil
}

// onUserJoined 用户加入群组时触发（service message 方式，部分群组可能不触发）
func (h *Handler) onUserJoined(c tele.Context) error {
	user := c.Message().UserJoined
	if user == nil {
		user = c.Sender()
	}
	if user == nil || user.IsBot {
		return nil
	}

	slog.Info("新用户加入群组",
		"chat_id", c.Chat().ID,
		"user_id", user.ID,
		"username", user.Username,
	)

	group, ok := h.groups[c.Chat().ID]
	if !ok {
		return nil
	}
	h.handleJoin(c.Chat(), user, group)
	return nil
}

// verifyTimeout 等待 wait 后仍未验证则踢出；窗口内多次超时改为封禁较长时间
func (h *Handler) verifyTimeout(userID int64, wait time.Duration) {
	defer logPanic("验证超时处理")
	if wait > 0 {
		time.Sleep(wait)
	}

	h.mu.Lock()
	pv, exists := h.pending[userID]
	// 截止时间还没到，说明这是同一用户新一轮的验证，由新一轮的超时协程负责
	if !exists || time.Until(pv.Deadline) > time.Second {
		h.mu.Unlock()
		return
	}
	delete(h.pending, userID)
	h.mu.Unlock()
	h.savePending()

	chat := &tele.Chat{ID: pv.ChatID}
	user := &tele.User{ID: userID}

	member, mErr := h.bot.ChatMemberOf(chat, user)
	if mErr == nil && member.Role == tele.Kicked {
		slog.Info("用户已被其他来源封禁，跳过踢出操作", "user_id", userID)
		return
	}
	if mErr == nil && member.Role == tele.Left {
		slog.Info("用户已离开群组，无需踢出", "user_id", userID)
		if pv.MessageID != 0 {
			_ = h.bot.Delete(&tele.Message{ID: pv.MessageID, Chat: chat})
		}
		return
	}

	longBan := h.timeouts.add(userID) >= timeoutBanThreshold
	until := time.Now().Add(60 * time.Second)
	if longBan {
		until = time.Now().Add(timeoutBanDuration)
	}

	err := h.bot.Ban(chat, &tele.ChatMember{
		User:            user,
		RestrictedUntil: until.Unix(),
	})
	if err != nil {
		slog.Error("超时踢出失败", "user_id", userID, "error", err)
		return
	}
	if !longBan {
		time.Sleep(500 * time.Millisecond)
		if err := h.bot.Unban(chat, user); err != nil {
			slog.Warn("Unban失败，重试一次", "user_id", userID, "error", err)
			time.Sleep(1 * time.Second)
			if err := h.bot.Unban(chat, user); err != nil {
				slog.Error("Unban再次失败，将在60秒后自动解除", "user_id", userID, "error", err)
			}
		}
	} else {
		h.timeouts.reset(userID)
	}

	h.Invalidate(pv.ChatID, userID)
	slog.Info("用户验证超时，已踢出", "user_id", userID, "chat_id", pv.ChatID, "long_ban", longBan)

	if pv.MessageID != 0 {
		_ = h.bot.Delete(&tele.Message{ID: pv.MessageID, Chat: chat})
	}

	kickText := fmt.Sprintf("⏰ [%s](tg://user?id=%d) 验证超时，已被移出群组。", mdEscape(pv.DisplayName), userID)
	if longBan {
		kickText = fmt.Sprintf("⛔ [%s](tg://user?id=%d) 多次验证超时，已被封禁 24 小时。", mdEscape(pv.DisplayName), userID)
	}
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
	if err := c.Send(h.buildStartMessage(), tele.ModeMarkdown); err != nil {
		return err
	}
	if n := h.approvePendingJoins(c.Sender().ID); n > 0 {
		return c.Send("✅ 已自动通过您的入群申请，欢迎加入！")
	}
	return nil
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

	client, user, bound, err := h.lookupBoundUser(pv.ChatID, userID)
	if client == nil {
		return c.Send("❌ 系统错误，请联系管理员。")
	}
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
	if err != nil || user == nil {
		b, _ := h.bindings.Get(userID)
		slog.Warn("本地绑定的邮箱在数据库中未找到",
			"telegram_id", userID, "email", b.Email, "error", err,
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
	h.savePending()

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
	h.sendWelcome(chat, userID, name, planName)

	slog.Info("用户验证通过", "user_id", userID, "chat_id", pv.ChatID)
}

// findUserAnyDB 在所有群对应的数据库中按邮箱查找用户，返回用户与所在数据库名
func (h *Handler) findUserAnyDB(email string) (*db.V2User, string) {
	for chatID, client := range h.dbClients {
		u, err := client.FindUserByEmail(email)
		if err != nil {
			slog.Error("查询用户失败", "email", email, "error", err)
			continue
		}
		if u != nil {
			return u, h.groups[chatID].Database.DBName
		}
	}
	return nil, ""
}

// checkPassword 校验面板密码，限制并发以免 bcrypt 占满 CPU
func (h *Handler) checkPassword(u *db.V2User, password string) bool {
	h.pwSem <- struct{}{}
	defer func() { <-h.pwSem }()
	return db.VerifyPassword(u.PasswordAlgo.String, u.PasswordSalt.String, password, u.Password)
}

// bindReply 绑定类命令的回复，带上"消息已删除"提示
type bindReply struct {
	c       tele.Context
	deleted bool
}

func (r bindReply) send(text string) error {
	if r.deleted {
		text += "\n\n🔒 为保护密码安全，您发送的绑定消息已自动删除。"
	}
	return r.c.Send(text)
}

// prepareBind 绑定类命令的公共前置：删除含密码的消息、检查锁定
// ok=false 时已回复用户，调用方直接返回
func (h *Handler) prepareBind(c tele.Context, args []string) (reply bindReply, ok bool, err error) {
	reply = bindReply{c: c}
	if len(args) >= 3 {
		if dErr := h.bot.Delete(c.Message()); dErr == nil {
			reply.deleted = true
		} else {
			slog.Debug("删除绑定消息失败", "user_id", c.Sender().ID, "error", dErr)
		}
	}
	if wait := h.bindLimit.lockedFor(c.Sender().ID); wait > 0 {
		minutes := int(wait.Minutes()) + 1
		return reply, false, reply.send(fmt.Sprintf("⛔ 绑定尝试失败次数过多，请 %d 分钟后再试。", minutes))
	}
	return reply, true, nil
}

// bindFailed 记录一次绑定失败并回复剩余次数或锁定提示
func (h *Handler) bindFailed(reply bindReply, userID int64, text string) error {
	left := h.bindLimit.fail(userID)
	if left == 0 {
		slog.Warn("绑定失败次数过多，已锁定", "user_id", userID, "lock", bindLockDuration)
		return reply.send(text + fmt.Sprintf("\n\n⛔ 失败次数过多，已锁定 %d 分钟。", int(bindLockDuration.Minutes())))
	}
	return reply.send(text + fmt.Sprintf("\n\n还可尝试 %d 次。", left))
}

// verifyBindCredentials 校验邮箱与密码，成功返回数据库中的用户与库名；失败时已回复用户
func (h *Handler) verifyBindCredentials(reply bindReply, userID int64, email, password string) (*db.V2User, string, error) {
	user, dbName := h.findUserAnyDB(email)
	if user == nil {
		return nil, "", h.bindFailed(reply, userID, "❌ 未找到该邮箱对应的账户，请检查邮箱是否正确。")
	}
	if !h.checkPassword(user, password) {
		return nil, "", h.bindFailed(reply, userID, "❌ 密码错误，请重试。")
	}
	h.bindLimit.reset(userID)
	if user.Banned != 0 {
		return nil, "", reply.send("❌ 您的账户已被封禁，无法绑定。")
	}
	return user, dbName, nil
}

// onBind 处理 /bind 命令
func (h *Handler) onBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	args := strings.Fields(c.Message().Text)
	reply, ok, err := h.prepareBind(c, args)
	if !ok {
		return err
	}
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/bind 邮箱 密码`\n\n示例：`/bind user@example.com mypassword`",
			tele.ModeMarkdown,
		)
	}

	userID := c.Sender().ID
	foundUser, foundDBName, err := h.verifyBindCredentials(reply, userID, args[1], args[2])
	if foundUser == nil {
		return err
	}
	// 以数据库中的邮箱为准，避免大小写不同导致重复绑定
	email := foundUser.Email

	if existingTG := h.bindings.FindByEmail(email); existingTG != 0 && existingTG != userID {
		return reply.send("❌ 该邮箱已被其他 Telegram 账号绑定。\n如需更换，请先在原账号执行 /unbind 解绑，或使用 /forcebind 强制绑定。")
	}

	if err := h.bindings.Set(userID, email, foundDBName); err != nil {
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return reply.send("❌ 绑定失败，请稍后重试。")
	}

	slog.Info("用户绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)
	return reply.send(h.afterBind(userID, email, foundUser, "✅ 绑定成功！"))
}

// onForceBind 强制绑定：通过邮箱+密码验证身份后，解除该邮箱的旧绑定并绑定到当前 TG 号
func (h *Handler) onForceBind(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	args := strings.Fields(c.Message().Text)
	reply, ok, err := h.prepareBind(c, args)
	if !ok {
		return err
	}
	if len(args) != 3 {
		return c.Send(
			"📝 用法：`/forcebind 邮箱 密码`\n\n"+
				"用于强制将邮箱绑定到当前 Telegram 账号。\n"+
				"如果该邮箱已被其他 TG 号绑定，验证密码后会自动解除旧绑定。",
			tele.ModeMarkdown,
		)
	}

	userID := c.Sender().ID
	foundUser, foundDBName, err := h.verifyBindCredentials(reply, userID, args[1], args[2])
	if foundUser == nil {
		return err
	}
	email := foundUser.Email

	oldTG := h.bindings.FindByEmail(email)
	rebound := oldTG != 0 && oldTG != userID
	if rebound {
		_ = h.bindings.Delete(oldTG)
		for chatID := range h.verifyCache {
			h.Invalidate(chatID, oldTG)
		}
		slog.Info("强制解除旧绑定", "email", email, "old_tg", oldTG, "new_tg", userID)
		_, nErr := h.bot.Send(&tele.User{ID: oldTG}, fmt.Sprintf(
			"⚠️ 您绑定的面板账户 %s 已被另一个 Telegram 账号通过邮箱和密码重新绑定，当前账号的绑定已解除。\n\n"+
				"如果不是您本人操作，说明面板密码可能已泄露，请尽快登录官网修改密码。",
			maskEmail(email),
		))
		if nErr != nil {
			slog.Warn("通知原绑定账号失败", "old_tg", oldTG, "error", nErr)
		}
	}

	if err := h.bindings.Set(userID, email, foundDBName); err != nil {
		slog.Error("保存绑定失败", "user_id", userID, "error", err)
		return reply.send("❌ 绑定失败，请稍后重试。")
	}

	slog.Info("用户强制绑定成功", "telegram_id", userID, "email", email, "db", foundDBName)
	title := "✅ 绑定成功！"
	if rebound {
		title = "✅ 已解除旧绑定并重新绑定成功！"
	}
	return reply.send(h.afterBind(userID, email, foundUser, title))
}

// afterBind 绑定成功后的公共流程：完成待验证、批准入群申请，返回回复文案
func (h *Handler) afterBind(userID int64, email string, foundUser *db.V2User, title string) string {
	h.mu.RLock()
	pv, hasPending := h.pending[userID]
	h.mu.RUnlock()

	if hasPending {
		// 必须用待验证群组对应的数据库来判断，而不是绑定时搜到的数据库
		if groupDB, ok := h.dbClients[pv.ChatID]; ok {
			groupUser, err := groupDB.FindUserByEmail(email)
			if err == nil && groupUser != nil && db.IsUserValid(groupUser) {
				slog.Info("绑定时自动审批通过",
					"telegram_id", userID, "email", email,
					"plan_id", groupUser.PlanID, "expired_at", groupUser.ExpiredAt,
				)
				h.approveUser(userID, planNameOf(groupDB, groupUser))
				return fmt.Sprintf("%s\n\n邮箱：%s\n已自动完成验证并解除禁言 🎉", title, email)
			}
			reason := describeInvalid(groupUser)
			slog.Info("绑定成功但套餐无效，未审批",
				"telegram_id", userID, "email", email, "reason", reason,
			)
			return fmt.Sprintf("%s\n\n但验证未通过：%s\n请前往官网购买/续费套餐。", title, reason)
		}
	}

	if !db.IsUserValid(foundUser) {
		return title + "\n\n但您当前没有有效套餐，请前往官网购买套餐后加入群组。"
	}
	if h.approvePendingJoins(userID) > 0 {
		return fmt.Sprintf("%s\n\n邮箱：%s\n已自动通过您的入群申请 🎉", title, email)
	}
	return fmt.Sprintf("%s\n\n邮箱：%s\n现在您可以加入群组了。", title, email)
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
	if err := c.Send(text, tele.ModeMarkdown); err != nil {
		return err
	}
	if h.approvePendingJoins(userID) > 0 {
		return c.Send("✅ 已自动通过您的入群申请，欢迎加入！")
	}
	return nil
}

// onCheck 管理员手动触发一次巡检
func (h *Handler) onCheck(c tele.Context) error {
	if isGroupChat(c) {
		return h.rejectInGroup(c)
	}
	if !h.config.IsAdmin(c.Sender().ID) {
		return c.Send("❓ 无效命令，请输入 /start 查看帮助。")
	}
	if h.triggerCheck == nil {
		return c.Send("❌ 巡检功能未启用。")
	}
	if !h.triggerCheck() {
		return c.Send("⏳ 已有巡检正在进行，请稍后查看日志。")
	}
	return c.Send("✅ 已开始巡检，结果请查看日志中的\"巡检完成\"记录。")
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
