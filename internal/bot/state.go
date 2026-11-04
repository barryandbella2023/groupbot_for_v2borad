package bot

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"v2board-tg-bot/internal/atomicfile"
)

// savePending 把待验证列表写入磁盘，重启后可恢复超时处理
func (h *Handler) savePending() {
	if h.pendingPath == "" {
		return
	}
	h.mu.RLock()
	list := make([]PendingVerify, 0, len(h.pending))
	for _, pv := range h.pending {
		list = append(list, *pv)
	}
	h.mu.RUnlock()

	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if err := atomicfile.WriteJSON(h.pendingPath, list); err != nil {
		slog.Error("保存待验证列表失败", "path", h.pendingPath, "error", err)
	}
}

// RestorePending 启动时恢复上次未完成的验证，并按剩余时间重新安排超时处理
func (h *Handler) RestorePending() {
	if h.pendingPath == "" {
		return
	}
	raw, err := os.ReadFile(h.pendingPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("读取待验证列表失败", "path", h.pendingPath, "error", err)
		}
		return
	}
	var list []PendingVerify
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &list); err != nil {
			slog.Error("解析待验证列表失败", "path", h.pendingPath, "error", err)
			return
		}
	}

	h.mu.Lock()
	for i := range list {
		pv := list[i]
		h.pending[pv.UserID] = &pv
	}
	h.mu.Unlock()

	for _, pv := range list {
		go h.verifyTimeout(pv.UserID, time.Until(pv.Deadline))
	}
	if len(list) > 0 {
		slog.Info("已恢复待验证用户", "count", len(list))
	}
}
