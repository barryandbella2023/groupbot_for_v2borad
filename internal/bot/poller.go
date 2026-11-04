package bot

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	tele "gopkg.in/telebot.v3"
)

const (
	pollBackoffMin = 1 * time.Second
	pollBackoffMax = 30 * time.Second
	// 无法解析的更新在日志中保留的原始内容长度
	badUpdateSnippetLen = 2000
)

// SafePoller 替代 tele.LongPoller：
// - 拉取失败时记录错误并退避重试，而不是静默地立即重试
// - 逐条解析更新，单条无法解析时记录并跳过，不会卡住整个队列
type SafePoller struct {
	Timeout        time.Duration
	AllowedUpdates []string

	lastUpdateID int
}

// Poll 实现 tele.Poller
func (p *SafePoller) Poll(b *tele.Bot, dest chan tele.Update, stop chan struct{}) {
	backoff := pollBackoffMin
	failures := 0
	var failingSince time.Time

	for {
		select {
		case <-stop:
			return
		default:
		}

		updates, err := p.fetch(b)
		if err != nil {
			if failures == 0 {
				failingSince = time.Now()
			}
			failures++

			wait := backoff
			var flood tele.FloodError
			if errors.As(err, &flood) && flood.RetryAfter > 0 {
				wait = time.Duration(flood.RetryAfter) * time.Second
			} else {
				backoff = min(backoff*2, pollBackoffMax)
			}
			slog.Error("拉取 Telegram 更新失败",
				"error", err, "consecutive_failures", failures, "retry_in", wait,
			)

			select {
			case <-stop:
				return
			case <-time.After(wait):
			}
			continue
		}

		if failures > 0 {
			slog.Info("拉取 Telegram 更新已恢复",
				"failed_times", failures, "outage", time.Since(failingSince).Round(time.Second),
			)
			failures = 0
			backoff = pollBackoffMin
		}

		for _, u := range updates {
			dest <- u
		}
	}
}

// fetch 调用 getUpdates 并逐条解析，返回可处理的更新
func (p *SafePoller) fetch(b *tele.Bot) ([]tele.Update, error) {
	allowed, _ := json.Marshal(p.AllowedUpdates)
	params := map[string]string{
		"offset":          strconv.Itoa(p.lastUpdateID + 1),
		"timeout":         strconv.Itoa(int(p.Timeout / time.Second)),
		"allowed_updates": string(allowed),
	}

	data, err := b.Raw("getUpdates", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	updates := make([]tele.Update, 0, len(resp.Result))
	for _, raw := range resp.Result {
		var u tele.Update
		if err := json.Unmarshal(raw, &u); err != nil {
			var head struct {
				ID int `json:"update_id"`
			}
			_ = json.Unmarshal(raw, &head)
			if head.ID > p.lastUpdateID {
				p.lastUpdateID = head.ID
			}
			snippet := string(raw)
			if len(snippet) > badUpdateSnippetLen {
				snippet = snippet[:badUpdateSnippetLen]
			}
			slog.Error("无法解析的更新，已跳过",
				"update_id", head.ID, "error", err, "raw", snippet,
			)
			continue
		}
		if u.ID > p.lastUpdateID {
			p.lastUpdateID = u.ID
		}
		updates = append(updates, u)
	}
	return updates, nil
}
