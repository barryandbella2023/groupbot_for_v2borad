package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	tele "gopkg.in/telebot.v3"

	"v2board-tg-bot/internal/binding"
	"v2board-tg-bot/internal/bot"
	"v2board-tg-bot/internal/config"
	"v2board-tg-bot/internal/db"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	bindingPath := flag.String("bindings", "bindings.json", "绑定数据文件路径")
	debug := flag.Bool("debug", false, "开启 DEBUG 日志级别")
	flag.Parse()

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	slog.Info("V2Board 群组管理机器人启动中...")

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("加载配置失败", "error", err)
		os.Exit(1)
	}
	slog.Info("配置加载成功", "groups", len(cfg.Groups), "check_interval", cfg.CheckInterval)

	bindings, err := binding.New(*bindingPath)
	if err != nil {
		slog.Error("加载绑定数据失败", "error", err)
		os.Exit(1)
	}
	slog.Info("绑定数据加载成功", "path", *bindingPath)

	dbClients := make(map[int64]*db.Client)
	for _, g := range cfg.Groups {
		client, err := db.New(g.Database)
		if err != nil {
			slog.Error("数据库连接失败", "chat_id", g.ChatID, "error", err)
			os.Exit(1)
		}
		dbClients[g.ChatID] = client
	}
	defer func() {
		for _, c := range dbClients {
			_ = c.Close()
		}
	}()

	seedBindingsOnce(*bindingPath, bindings, dbClients, cfg)

	b, err := tele.NewBot(tele.Settings{
		Token: cfg.Telegram.BotToken,
		Poller: &bot.SafePoller{
			Timeout:        30 * time.Second,
			AllowedUpdates: tele.AllowedUpdates,
		},
	})
	if err != nil {
		slog.Error("创建 Bot 失败", "error", err)
		os.Exit(1)
	}
	slog.Info("Bot 连接成功", "username", b.Me.Username)

	applyBotProfile(b, cfg.Telegram.Profile)

	handler := bot.NewHandler(b, cfg, dbClients, bindings)
	handler.Register()

	checker := bot.NewChecker(b, cfg, dbClients, bindings, handler.Invalidate)
	handler.OnPresent(checker.MarkPresent)
	go checker.Start()

	slog.Info("机器人已启动，等待事件...")
	b.Start()
}

// seedBindingsOnce 仅首次启动时把所有数据库中已绑定的 telegram_id 一次性导入本地
// 通过 <bindings_path>.imported 标记文件防止重复导入
// 导入完成后，bot 完全以本地 bindings.json 为权威源，不再读取 v2_user.telegram_id 字段
func seedBindingsOnce(bindingPath string, store *binding.Store, clients map[int64]*db.Client, cfg *config.Config) {
	marker := bindingPath + ".imported"
	if _, err := os.Stat(marker); err == nil {
		slog.Info("绑定种子已导入过，跳过", "marker", marker)
		return
	}

	slog.Info("首次启动：开始从数据库导入历史绑定...")
	totalAdded := 0
	totalSkipped := 0

	for _, g := range cfg.Groups {
		client, ok := clients[g.ChatID]
		if !ok {
			continue
		}
		rows, err := client.ListAllTelegramBindings()
		if err != nil {
			slog.Error("导出绑定失败", "db", g.Database.DBName, "error", err)
			continue
		}
		added, skipped := 0, 0
		for tgID, email := range rows {
			if store.SetIfAbsent(tgID, email, g.Database.DBName) {
				added++
			} else {
				skipped++
				slog.Debug("绑定冲突跳过", "telegram_id", tgID, "email", email, "db", g.Database.DBName)
			}
		}
		slog.Info("导入完成", "db", g.Database.DBName, "added", added, "skipped", skipped, "scanned", len(rows))
		totalAdded += added
		totalSkipped += skipped
	}

	if err := store.SaveNow(); err != nil {
		slog.Error("保存绑定文件失败", "error", err)
		return
	}

	if dir := filepath.Dir(marker); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	if err := os.WriteFile(marker, []byte(time.Now().Format(time.RFC3339)), 0644); err != nil {
		slog.Warn("写入导入标记失败", "marker", marker, "error", err)
	}
	slog.Info("绑定种子导入完成", "added", totalAdded, "skipped", totalSkipped, "marker", marker)
}

// applyBotProfile 应用 Bot 的介绍卡片与命令菜单设置
func applyBotProfile(b *tele.Bot, p config.BotProfileConfig) {
	if p.Description != "" {
		if err := b.SetMyDescription(p.Description, ""); err != nil {
			slog.Warn("设置 Bot 介绍失败", "error", err)
		} else {
			slog.Info("Bot 介绍已更新")
		}
	}
	if p.ShortDescription != "" {
		if err := b.SetMyShortDescription(p.ShortDescription, ""); err != nil {
			slog.Warn("设置 Bot 简介失败", "error", err)
		} else {
			slog.Info("Bot 简介已更新")
		}
	}
	if len(p.Commands) > 0 {
		cmds := make([]tele.Command, 0, len(p.Commands))
		for _, c := range p.Commands {
			cmds = append(cmds, tele.Command{Text: c.Command, Description: c.Description})
		}
		if err := b.SetCommands(cmds); err != nil {
			slog.Warn("设置命令菜单失败", "error", err)
		} else {
			slog.Info("命令菜单已更新", "count", len(cmds))
		}
	}
}
