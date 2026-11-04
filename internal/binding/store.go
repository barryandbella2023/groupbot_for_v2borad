package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"v2board-tg-bot/internal/atomicfile"
)

// UserBinding 单个 Telegram 账号到面板账户的本地绑定
type UserBinding struct {
	Email  string `json:"email"`
	DBName string `json:"db_name"`
}

// Store 基于 JSON 文件的线程安全绑定存储
type Store struct {
	mu       sync.RWMutex
	filePath string
	// data key: telegram_id
	data map[int64]UserBinding
}

// New 打开（或初始化）绑定存储
// 文件不存在时从空数据开始；文件存在但无法读取或解析时返回错误，
// 避免以空数据启动后在下一次写入时覆盖掉全部绑定
func New(filePath string) (*Store, error) {
	s := &Store{
		filePath: filePath,
		data:     make(map[int64]UserBinding),
	}
	if err := s.load(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("绑定文件不存在，将创建新文件", "path", filePath)
			return s, nil
		}
		return nil, fmt.Errorf("读取绑定文件失败 [%s]: %w", filePath, err)
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, &s.data)
}

func (s *Store) save() error {
	return atomicfile.WriteJSON(s.filePath, s.data)
}

func (s *Store) Set(telegramID int64, email, dbName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[telegramID] = UserBinding{Email: email, DBName: dbName}
	return s.save()
}

// SetIfAbsent 仅当 telegram_id 不存在、且 email 未被其他 tg_id 占用时写入
// 返回值：写入是否实际发生（false 表示因冲突跳过）
// 注意：本方法不会立即落盘，需在批量调用后由调用方触发 SaveNow
func (s *Store) SetIfAbsent(telegramID int64, email, dbName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[telegramID]; exists {
		return false
	}
	for _, b := range s.data {
		if strings.EqualFold(b.Email, email) {
			return false
		}
	}
	s.data[telegramID] = UserBinding{Email: email, DBName: dbName}
	return true
}

// SaveNow 立即把当前内存状态写入磁盘，用于批量 SetIfAbsent 之后
func (s *Store) SaveNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// Has 判断某个 telegram_id 是否已有本地绑定
func (s *Store) Has(telegramID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[telegramID]
	return ok
}

func (s *Store) Get(telegramID int64) (UserBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data[telegramID]
	return b, ok
}

func (s *Store) Delete(telegramID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, telegramID)
	return s.save()
}

// FindByEmail 检查某个邮箱是否已被其他 Telegram 账号绑定，返回 telegram_id；未找到返回 0
// 邮箱比较不区分大小写，与 MySQL 默认排序规则一致
func (s *Store) FindByEmail(email string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for tgID, b := range s.data {
		if strings.EqualFold(b.Email, email) {
			return tgID
		}
	}
	return 0
}

// GetAllForDB 获取某个数据库下所有绑定的 telegram_id
func (s *Store) GetAllForDB(dbName string) map[int64]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[int64]string)
	for tgID, b := range s.data {
		if b.DBName == dbName {
			result[tgID] = b.Email
		}
	}
	return result
}
