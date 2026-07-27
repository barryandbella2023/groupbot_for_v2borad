package bot

import (
	"fmt"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/db"
)

// planNameOf 返回用户的套餐名，若未找到返回空字符串
func planNameOf(client *db.Client, user *db.V2User) string {
	if user == nil || !user.PlanID.Valid || user.PlanID.Int64 == 0 {
		return ""
	}
	return client.FindPlanNameByID(user.PlanID.Int64)
}

// describeInvalid 生成无效套餐的原因描述
func describeInvalid(user *db.V2User) string {
	if user == nil {
		return "未找到账户"
	}
	if user.Banned != 0 {
		return "账户已被封禁"
	}
	if !user.PlanID.Valid || user.PlanID.Int64 == 0 {
		return "尚未购买套餐"
	}
	now := time.Now().Unix()
	if user.ExpiredAt.Valid && user.ExpiredAt.Int64 != 0 && user.ExpiredAt.Int64 < now {
		return "套餐已过期"
	}
	if db.IsTrafficExhausted(user) {
		return "套餐流量已用尽"
	}
	return "套餐无效"
}

// formatBytes 把字节数格式化为人类可读字符串
func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
		tb = gb * 1024
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.2f TB", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// trafficLine 生成"流量：已用 X / 总 Y (Z%) [类型]"格式的描述
// 不限流量返回"流量：不限"
func trafficLine(user *db.V2User) string {
	if user == nil {
		return "流量：未知"
	}
	if !user.TransferEnable.Valid || user.TransferEnable.Int64 <= 0 {
		return "流量：不限"
	}
	used := int64(0)
	if user.U.Valid {
		used += user.U.Int64
	}
	if user.D.Valid {
		used += user.D.Int64
	}
	total := user.TransferEnable.Int64
	pct := float64(used) * 100 / float64(total)
	return fmt.Sprintf("流量：%s / %s (%.1f%%) · %s",
		formatBytes(used), formatBytes(total), pct, planResetDesc(user))
}

// planResetDesc 把 reset_traffic_method 翻译为人类可读的套餐类型
func planResetDesc(user *db.V2User) string {
	if user == nil || !user.PlanResetMethod.Valid {
		return "周期重置"
	}
	switch user.PlanResetMethod.Int64 {
	case 0:
		return "月初重置"
	case 1:
		return "到期日重置"
	case 2:
		return "一次性套餐"
	case 3:
		return "年初重置"
	case 4:
		return "年到期日重置"
	default:
		return "周期重置"
	}
}

// displayName 生成 Telegram 用户的显示名称
func displayName(u *tele.User) string {
	name := u.FirstName
	if u.LastName != "" {
		name += " " + u.LastName
	}
	if name == "" {
		name = u.Username
	}
	if name == "" {
		name = fmt.Sprintf("用户%d", u.ID)
	}
	return name
}

// mdEscape 转义 Telegram Markdown(v1) 中会触发实体解析的字符
// 需要处理：_ * ` [
func mdEscape(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"`", "\\`",
		"[", "\\[",
	)
	return replacer.Replace(s)
}

// formatUserInfo 详细用户卡片（用于 /cha）
func formatUserInfo(client *db.Client, user *db.V2User) string {
	status := userStatus(user)
	expiredStr := "永不过期"
	if user.ExpiredAt.Valid && user.ExpiredAt.Int64 != 0 {
		expiredStr = time.Unix(user.ExpiredAt.Int64, 0).Format("2006-01-02 15:04:05")
	}
	planStr := "无"
	if user.PlanID.Valid && user.PlanID.Int64 != 0 {
		if name := client.FindPlanNameByID(user.PlanID.Int64); name != "" {
			planStr = fmt.Sprintf("%s (ID: %d)", mdEscape(name), user.PlanID.Int64)
		} else {
			planStr = fmt.Sprintf("ID: %d", user.PlanID.Int64)
		}
	}
	return fmt.Sprintf(
		"👤 ID：%d\n📧 邮箱：`%s`\n📦 套餐：%s\n⏰ 到期：%s\n📊 %s\n📌 状态：%s",
		user.ID, user.Email, planStr, expiredStr, trafficLine(user), status,
	)
}

// userStatus 把用户当前的有效性映射为带 emoji 的简短状态文案
func userStatus(user *db.V2User) string {
	if db.IsUserValid(user) {
		return "✅ 有效"
	}
	switch {
	case user == nil:
		return "❓ 未知"
	case user.Banned != 0:
		return "🚫 已封禁"
	case !user.PlanID.Valid || user.PlanID.Int64 == 0:
		return "❌ 无套餐"
	case user.ExpiredAt.Valid && user.ExpiredAt.Int64 != 0 && user.ExpiredAt.Int64 < time.Now().Unix():
		return "❌ 已过期"
	case db.IsTrafficExhausted(user):
		return "❌ 流量耗尽"
	default:
		return "❌ 无效"
	}
}

// maskEmail 对邮箱进行脱敏：ab***@xx.com
func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 {
		return "***"
	}
	local := email[:at]
	domain := email[at:]
	if len(local) <= 2 {
		return local[:1] + "***" + domain
	}
	return local[:2] + "***" + domain
}

// formatStatusLine 精简用户卡片（用于 /status），mask=true 时邮箱脱敏
func formatStatusLine(client *db.Client, user *db.V2User, mask bool) string {
	status := userStatus(user)
	planStr := "无"
	if user.PlanID.Valid && user.PlanID.Int64 != 0 {
		if name := client.FindPlanNameByID(user.PlanID.Int64); name != "" {
			planStr = mdEscape(name)
		} else {
			planStr = fmt.Sprintf("ID %d", user.PlanID.Int64)
		}
	}
	expiredStr := "永不过期"
	if user.ExpiredAt.Valid && user.ExpiredAt.Int64 != 0 {
		expiredStr = time.Unix(user.ExpiredAt.Int64, 0).Format("2006-01-02 15:04")
	}
	email := user.Email
	if mask {
		email = maskEmail(email)
	}
	return fmt.Sprintf(
		"👤 邮箱：`%s`\n📦 套餐：%s\n⏰ 到期：%s\n📊 %s\n📌 状态：%s",
		email, planStr, expiredStr, trafficLine(user), status,
	)
}
