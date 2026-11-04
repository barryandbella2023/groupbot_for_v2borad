package bot

import (
	"fmt"
	"log/slog"
	"time"

	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/config"
	"v2board-tg-bot/internal/db"
)

// lookupBoundUser 查询用户绑定在该群对应数据库中的账户
// bound=false 表示没有绑定，或绑定在其他群的数据库
func (h *Handler) lookupBoundUser(chatID, userID int64) (client *db.Client, user *db.V2User, bound bool, err error) {
	client, ok := h.dbClients[chatID]
	group, gok := h.groups[chatID]
	if !ok || !gok {
		return nil, nil, false, nil
	}
	b, has := h.bindings.Get(userID)
	if !has || b.DBName != group.Database.DBName {
		return client, nil, false, nil
	}
	user, err = client.FindUserByEmail(b.Email)
	return client, user, true, err
}

// onJoinRequest 处理入群申请：已绑定且套餐有效的用户自动批准，其余保留申请并私信提示
// 需要群开启"审核新成员"，且 bot 拥有邀请用户权限
func (h *Handler) onJoinRequest(c tele.Context) error {
	req := c.ChatJoinRequest()
	if req == nil || req.Chat == nil || req.Sender == nil {
		return nil
	}
	group, ok := h.groups[req.Chat.ID]
	if !ok {
		return nil
	}
	chat, user := req.Chat, req.Sender

	if group.IsExempt(user.ID) || h.config.IsAdmin(user.ID) {
		h.approveJoin(chat, user, "白名单")
		return nil
	}

	_, u, bound, err := h.lookupBoundUser(chat.ID, user.ID)
	if err != nil {
		// 查询失败时保留申请，交给后续重试或管理员处理
		slog.Error("处理入群申请时查询用户失败", "chat_id", chat.ID, "user_id", user.ID, "error", err)
		return nil
	}
	if bound && db.IsUserValid(u) {
		h.approveJoin(chat, user, "套餐有效")
		return nil
	}

	reason := "未绑定"
	var text string
	if !bound {
		text = "📝 收到您的入群申请。\n\n" +
			"请先绑定面板账户：`/bind 邮箱 密码`\n\n" +
			"绑定成功且套餐有效后，将自动通过您的申请。"
	} else {
		reason = describeInvalid(u)
		text = fmt.Sprintf("❌ 收到您的入群申请，但%s。\n\n"+
			"请前往官网续费后，私聊我发送 /start，即可自动通过申请。", reason)
	}
	slog.Info("入群申请暂不批准", "chat_id", chat.ID, "user_id", user.ID, "reason", reason)

	// user_chat_id 允许在申请处理前 5 分钟内私信从未启动过 bot 的用户
	to := &tele.Chat{ID: req.UserChatID}
	if req.UserChatID == 0 {
		to = &tele.Chat{ID: user.ID}
	}
	if _, err := h.bot.Send(to, text, tele.ModeMarkdown); err != nil {
		slog.Warn("私信入群申请人失败", "user_id", user.ID, "error", err)
	}
	return nil
}

func (h *Handler) approveJoin(chat *tele.Chat, user *tele.User, reason string) {
	if err := h.bot.ApproveJoinRequest(chat, user); err != nil {
		slog.Error("批准入群申请失败", "chat_id", chat.ID, "user_id", user.ID, "error", err)
		return
	}
	h.markVerified(chat.ID, user.ID)
	slog.Info("已批准入群申请", "chat_id", chat.ID, "user_id", user.ID, "reason", reason)
}

// approvePendingJoins 尝试批准该用户在各群中待处理的入群申请，返回成功批准的数量
// 不在本地记录申请：直接调用批准接口，没有申请时 Telegram 会返回错误，忽略即可
func (h *Handler) approvePendingJoins(userID int64) int {
	approved := 0
	user := &tele.User{ID: userID}
	for chatID, group := range h.groups {
		if !group.IsExempt(userID) && !h.config.IsAdmin(userID) {
			_, u, bound, err := h.lookupBoundUser(chatID, userID)
			if err != nil || !bound || !db.IsUserValid(u) {
				continue
			}
		}
		if err := h.bot.ApproveJoinRequest(&tele.Chat{ID: chatID}, user); err != nil {
			slog.Debug("无待批准的入群申请", "chat_id", chatID, "user_id", userID, "error", err)
			continue
		}
		h.markVerified(chatID, userID)
		approved++
		slog.Info("已批准入群申请", "chat_id", chatID, "user_id", userID, "reason", "绑定或续费后自动批准")
	}
	return approved
}

// handleJoin 用户进入群组后：白名单与已绑定有效用户直接放行，其余启动验证
func (h *Handler) handleJoin(chat *tele.Chat, user *tele.User, group *config.GroupConfig) {
	if !h.joinDedup.first(chat.ID, user.ID) {
		return
	}
	h.notifyPresent(chat.ID, user.ID)

	if group.IsExempt(user.ID) || h.config.IsAdmin(user.ID) {
		slog.Info("白名单用户，跳过验证", "user_id", user.ID)
		h.markVerified(chat.ID, user.ID)
		_, _ = h.bot.Send(chat,
			fmt.Sprintf("👋 欢迎 [%s](tg://user?id=%d) 加入！", mdEscape(displayName(user)), user.ID),
			tele.ModeMarkdown,
		)
		return
	}

	client, u, bound, err := h.lookupBoundUser(chat.ID, user.ID)
	if err == nil && bound && db.IsUserValid(u) {
		h.markVerified(chat.ID, user.ID)
		h.sendWelcome(chat, user.ID, displayName(user), planNameOf(client, u))
		slog.Info("已绑定的有效用户入群，直接放行", "chat_id", chat.ID, "user_id", user.ID)
		return
	}

	h.startVerify(chat, user, group, true)
}

// sendWelcome 在群内发送欢迎消息，60 秒后自动删除
func (h *Handler) sendWelcome(chat *tele.Chat, userID int64, name, planName string) {
	safeName := mdEscape(name)
	var text string
	if planName != "" {
		text = fmt.Sprintf("👏 欢迎 尊贵的 %s 用户 [%s](tg://user?id=%d)", mdEscape(planName), safeName, userID)
	} else {
		text = fmt.Sprintf("👏 欢迎 [%s](tg://user?id=%d)", safeName, userID)
	}
	msg, err := h.bot.Send(chat, text, tele.ModeMarkdown)
	if err != nil || msg == nil {
		return
	}
	go func() {
		time.Sleep(60 * time.Second)
		_ = h.bot.Delete(msg)
	}()
}
