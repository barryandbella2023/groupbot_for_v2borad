package bot

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const (
	// 限流状态最多保留的用户数，超出后淘汰最久未活动的
	limiterCapacity = 10000

	bindMaxFailures  = 5
	bindFailWindow   = 30 * time.Minute
	bindLockDuration = 30 * time.Minute

	privateMinInterval = 1 * time.Second

	timeoutBanThreshold = 3
	timeoutWindow       = 24 * time.Hour
	timeoutBanDuration  = 24 * time.Hour

	joinDedupWindow = 30 * time.Second
)

// windowCounter 统计每个用户在时间窗口内的事件次数
type windowCounter struct {
	mu     sync.Mutex
	window time.Duration
	data   *lru.Cache[int64, *windowEntry]
}

type windowEntry struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

func newWindowCounter(window time.Duration) *windowCounter {
	c, _ := lru.New[int64, *windowEntry](limiterCapacity)
	return &windowCounter{window: window, data: c}
}

// add 记录一次事件，返回窗口内累计次数
func (w *windowCounter) add(userID int64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	e, ok := w.data.Get(userID)
	if !ok || now.Sub(e.first) > w.window {
		e = &windowEntry{first: now}
		w.data.Add(userID, e)
	}
	e.count++
	return e.count
}

func (w *windowCounter) reset(userID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data.Remove(userID)
}

// bindLimiter 限制绑定命令的密码尝试：窗口内失败达到上限后锁定一段时间
type bindLimiter struct {
	failures *windowCounter
}

func newBindLimiter() *bindLimiter {
	return &bindLimiter{failures: newWindowCounter(bindFailWindow)}
}

// lockedFor 返回剩余锁定时长，0 表示未锁定
func (l *bindLimiter) lockedFor(userID int64) time.Duration {
	l.failures.mu.Lock()
	defer l.failures.mu.Unlock()
	e, ok := l.failures.data.Get(userID)
	if !ok {
		return 0
	}
	if d := time.Until(e.lockedUntil); d > 0 {
		return d
	}
	return 0
}

// fail 记录一次失败，返回剩余可尝试次数；为 0 表示已被锁定
func (l *bindLimiter) fail(userID int64) int {
	n := l.failures.add(userID)
	if n < bindMaxFailures {
		return bindMaxFailures - n
	}
	l.failures.mu.Lock()
	defer l.failures.mu.Unlock()
	if e, ok := l.failures.data.Get(userID); ok {
		e.lockedUntil = time.Now().Add(bindLockDuration)
		// 锁定期从现在开始计算，窗口也要覆盖锁定期，否则计数会被提前清零
		e.first = time.Now()
	}
	return 0
}

func (l *bindLimiter) reset(userID int64) {
	l.failures.reset(userID)
}

// rateGate 限制同一用户私聊消息的最小间隔
type rateGate struct {
	mu       sync.Mutex
	interval time.Duration
	last     *lru.Cache[int64, time.Time]
}

func newRateGate(interval time.Duration) *rateGate {
	c, _ := lru.New[int64, time.Time](limiterCapacity)
	return &rateGate{interval: interval, last: c}
}

func (g *rateGate) allow(userID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if t, ok := g.last.Get(userID); ok && now.Sub(t) < g.interval {
		return false
	}
	g.last.Add(userID, now)
	return true
}

// joinDedup 同一次入群会同时触发 chat_member 更新和入群服务消息，只处理第一个
type joinDedup struct {
	mu   sync.Mutex
	seen *lru.Cache[[2]int64, time.Time]
}

func newJoinDedup() *joinDedup {
	c, _ := lru.New[[2]int64, time.Time](limiterCapacity)
	return &joinDedup{seen: c}
}

// first 返回 true 表示这是窗口内该用户对该群的第一次入群事件
func (d *joinDedup) first(chatID, userID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := [2]int64{chatID, userID}
	now := time.Now()
	if t, ok := d.seen.Get(key); ok && now.Sub(t) < joinDedupWindow {
		return false
	}
	d.seen.Add(key, now)
	return true
}
