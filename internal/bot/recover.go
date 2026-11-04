package bot

import (
	"log/slog"
	"runtime/debug"

	tele "gopkg.in/telebot.v3"
)

// recoverMiddleware 拦截处理器中的 panic，避免单个请求导致整个进程退出
func recoverMiddleware() tele.MiddlewareFunc {
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("处理更新时发生 panic",
						"update_id", c.Update().ID, "panic", r, "stack", string(debug.Stack()),
					)
					err = nil
				}
			}()
			return next(c)
		}
	}
}

// logPanic 供后台协程使用：defer logPanic("任务名")
func logPanic(where string) {
	if r := recover(); r != nil {
		slog.Error("后台任务发生 panic", "where", where, "panic", r, "stack", string(debug.Stack()))
	}
}
