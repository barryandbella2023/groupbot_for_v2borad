package db

import (
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"

	"v2board-tg-bot/internal/config"
)

type V2User struct {
	ID             int64
	Email          string
	Password       string
	PasswordAlgo   sql.NullString
	PasswordSalt   sql.NullString
	PlanID         sql.NullInt64
	ExpiredAt      sql.NullInt64
	Banned         int
	TransferEnable sql.NullInt64 // 套餐总流量（字节），NULL 或 0 表示不限
	U              sql.NullInt64 // 已上传字节
	D              sql.NullInt64 // 已下载字节

	// PlanResetMethod 来自 v2_plan.reset_traffic_method，标识套餐流量重置方式
	// NULL 跟随全局配置 / 0 月初 / 1 到期日 / 2 不重置（一次性） / 3 年初 / 4 年到期日
	// 仅当此字段明确为 2（一次性套餐）时，流量耗尽才视为失效
	PlanResetMethod sql.NullInt64
}

type Client struct {
	db     *sql.DB
	config config.DatabaseConfig
}

// New 创建数据库客户端并校验连通性
func New(cfg config.DatabaseConfig) (*Client, error) {
	conn, err := sql.Open("mysql", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败 [%s]: %w", cfg.DBName, err)
	}

	conn.SetMaxOpenConns(3)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxLifetime(5 * time.Minute)
	// 须短于 MySQL wait_timeout，避免复用已被服务端关闭的空闲连接
	conn.SetConnMaxIdleTime(1 * time.Minute)

	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败 [%s]: %w", cfg.DBName, err)
	}

	slog.Info("数据库连接成功", "db", cfg.DBName, "host", cfg.Host)
	return &Client{db: conn, config: cfg}, nil
}

// 查询 SQL 模板：LEFT JOIN v2_plan 一并取出套餐的流量重置方式
// 不用 INNER JOIN 是为了 plan_id 为 NULL/0 的用户也能查到
const userSelectSQL = `
	SELECT u.id, u.email, u.password, u.password_algo, u.password_salt,
	       u.plan_id, u.expired_at, u.banned, u.transfer_enable, u.u, u.d,
	       p.reset_traffic_method
	FROM v2_user u
	LEFT JOIN v2_plan p ON p.id = u.plan_id
`

// FindUserByEmail 通过邮箱查找用户（只读）
func (c *Client) FindUserByEmail(email string) (*V2User, error) {
	return c.queryOne(userSelectSQL+" WHERE u.email = ? LIMIT 1", email)
}

// FindUserByTelegramID 通过 Telegram ID 查找用户（只读）
func (c *Client) FindUserByTelegramID(telegramID int64) (*V2User, error) {
	return c.queryOne(userSelectSQL+" WHERE u.telegram_id = ? LIMIT 1", telegramID)
}

func (c *Client) queryOne(query string, args ...any) (*V2User, error) {
	var u V2User
	err := c.db.QueryRow(query, args...).Scan(
		&u.ID, &u.Email,
		&u.Password, &u.PasswordAlgo, &u.PasswordSalt,
		&u.PlanID, &u.ExpiredAt, &u.Banned,
		&u.TransferEnable, &u.U, &u.D,
		&u.PlanResetMethod,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	return &u, nil
}

// FindPlanNameByID 通过套餐 ID 查询套餐名称，未找到返回空字符串
func (c *Client) FindPlanNameByID(planID int64) string {
	if planID == 0 {
		return ""
	}
	var name string
	err := c.db.QueryRow(`SELECT name FROM v2_plan WHERE id = ? LIMIT 1`, planID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// ListAllTelegramBindings 一次性导出 v2_user 中所有已绑定 telegram_id 的 (tg_id, email) 映射
// 用于 bot 启动时把历史绑定种子导入到本地 binding store，导入后 DB 不再被读取此字段
func (c *Client) ListAllTelegramBindings() (map[int64]string, error) {
	query := `
		SELECT telegram_id, email
		FROM v2_user
		WHERE telegram_id IS NOT NULL
		  AND telegram_id != 0
	`
	rows, err := c.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("导出 telegram_id 绑定失败: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]string)
	for rows.Next() {
		var tgID int64
		var email string
		if err := rows.Scan(&tgID, &email); err != nil {
			slog.Error("扫描绑定数据失败", "error", err)
			continue
		}
		result[tgID] = email
	}
	return result, rows.Err()
}

// GetExpiredTelegramUsers 获取数据库中绑定了 telegram_id 且套餐已过期的用户
// Deprecated: 巡检已改为遍历本地 bindings.json 并按 email 查 DB，此方法保留兼容

func (c *Client) GetExpiredTelegramUsers() (map[int64]string, error) {
	query := `
		SELECT telegram_id, email
		FROM v2_user
		WHERE telegram_id IS NOT NULL
		  AND telegram_id != 0
		  AND (
		    banned = 1
		    OR plan_id IS NULL
		    OR plan_id = 0
		    OR (expired_at IS NOT NULL AND expired_at != 0 AND expired_at < UNIX_TIMESTAMP())
		  )
	`
	rows, err := c.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("查询过期用户失败: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]string)
	for rows.Next() {
		var tgID int64
		var email string
		if err := rows.Scan(&tgID, &email); err != nil {
			slog.Error("扫描用户数据失败", "error", err)
			continue
		}
		result[tgID] = email
	}
	return result, rows.Err()
}

func (c *Client) Close() error {
	return c.db.Close()
}

// VerifyPassword 兼容 v2board 的多算法密码验证
func VerifyPassword(algo, salt, inputPassword, storedHash string) bool {
	switch algo {
	case "md5":
		h := md5.Sum([]byte(inputPassword))
		return hex.EncodeToString(h[:]) == storedHash
	case "sha256":
		h := sha256.Sum256([]byte(inputPassword))
		return hex.EncodeToString(h[:]) == storedHash
	case "md5salt":
		h := md5.Sum([]byte(inputPassword + salt))
		return hex.EncodeToString(h[:]) == storedHash
	default:
		return bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(inputPassword)) == nil
	}
}

// IsUserValid 检查用户套餐是否有效
func IsUserValid(user *V2User) bool {
	if user == nil || user.Banned != 0 {
		return false
	}
	if !user.PlanID.Valid || user.PlanID.Int64 == 0 {
		return false
	}
	now := time.Now().Unix()
	if user.ExpiredAt.Valid && user.ExpiredAt.Int64 != 0 && user.ExpiredAt.Int64 < now {
		return false
	}
	if IsTrafficExhausted(user) {
		return false
	}
	return true
}

// IsTrafficExhausted 判断"一次性套餐"用户流量是否已用尽
// 仅当 v2_plan.reset_traffic_method = 2（明确不重置，即一次性套餐）时才生效
// 周期套餐（月初/到期日/年初等）流量耗尽不视为失效，等流量重置或 expired_at 到期再处理
// transfer_enable 为 0 或 NULL 表示不限流量，永远视为未耗尽
func IsTrafficExhausted(user *V2User) bool {
	if user == nil {
		return false
	}
	if !user.PlanResetMethod.Valid || user.PlanResetMethod.Int64 != 2 {
		return false
	}
	if !user.TransferEnable.Valid || user.TransferEnable.Int64 <= 0 {
		return false
	}
	used := int64(0)
	if user.U.Valid {
		used += user.U.Int64
	}
	if user.D.Valid {
		used += user.D.Int64
	}
	return used >= user.TransferEnable.Int64
}
