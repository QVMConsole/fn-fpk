package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Role        string `json:"role"`
	Status      string `json:"status"`
	TOTPEnabled bool   `json:"totpEnabled"`
	Email       string `json:"email"`
}

type PasswordCheckResult struct {
	Prefix      string `json:"prefix"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
	Count       int    `json:"count"`
}

type AccountStore struct {
	cfg    Config
	client *http.Client
}

func NewAccountStore(cfg Config) *AccountStore {
	return &AccountStore{
		cfg:    cfg,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *AccountStore) open() (*sql.DB, error) {
	path := s.cfg.DatabasePath()
	if !fileExists(path) {
		return nil, fmt.Errorf("数据库不存在: %s", path)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("数据库不可访问: %w", err)
	}
	return db, nil
}

func (s *AccountStore) ListUsers(ctx context.Context) ([]User, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, username, role, status, totp_enabled, COALESCE(email, '') FROM users WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		var totp int
		if err := rows.Scan(&user.ID, &user.Username, &user.Role, &user.Status, &totp, &user.Email); err != nil {
			return nil, err
		}
		user.TOTPEnabled = totp == 1
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *AccountStore) ResetDefaultAdmin(ctx context.Context) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.backupDatabase(ctx, db, "reset-admin"); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin123"), 10)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE username = ? AND deleted_at IS NULL`, "admin").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO users (username, password_hash, role, status, cloud_type, created_at, updated_at) VALUES (?, ?, 'admin', 'active', 'elastic', datetime('now'), datetime('now'))`, "admin", string(hash))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=?, totp_enabled=0, totp_secret_enc='', totp_recovery_codes_enc='', totp_bound_at=NULL, email='', email_verified_at=NULL, updated_at=datetime('now') WHERE username=? AND deleted_at IS NULL`, string(hash), "admin")
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *AccountStore) ClearTOTP(ctx context.Context, userID int64) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.backupDatabase(ctx, db, "clear-totp"); err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE users SET totp_enabled=0, totp_secret_enc='', totp_recovery_codes_enc='', totp_bound_at=NULL, updated_at=datetime('now') WHERE id=? AND deleted_at IS NULL`, userID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return errors.New("用户不存在或已被删除")
	}
	return nil
}

func (s *AccountStore) ChangeAdminPassword(ctx context.Context, userID int64, password string, allowUnavailable bool) (PasswordCheckResult, error) {
	if len([]rune(password)) < 12 {
		return PasswordCheckResult{}, errors.New("密码长度不能少于 12 位")
	}
	secret := readEnvValue(s.cfg.EnvPath(), "KVM_SECURITY_SECRET")
	security := s.CheckPassword(ctx, password, secret)
	if security.Status == "breached" {
		return security, &AccountError{Code: "password_breached", Message: fmt.Sprintf("该密码已在公开泄露数据库中出现 %d 次", security.Count)}
	}
	if security.Status == "unavailable" && !allowUnavailable {
		return security, &AccountError{Code: "password_check_unavailable", Message: "密码泄露检测服务暂时不可用，需要额外确认"}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return security, err
	}
	db, err := s.open()
	if err != nil {
		return security, err
	}
	defer db.Close()
	if err := s.backupDatabase(ctx, db, "change-password"); err != nil {
		return security, err
	}
	result, err := db.ExecContext(ctx, `UPDATE users SET password_hash=?, password_breach_prefix=?, password_breach_hmac=?, password_breached=0, password_breach_count=0, password_breach_checked_at=NULL, password_breach_detected_at=NULL, password_breach_user_notified_at=NULL, password_breach_admin_notified_at=NULL, force_password_change=0, force_password_change_reason='', login_verified_until=NULL, high_risk_verified_until=NULL, security_updated_at=datetime('now'), updated_at=datetime('now') WHERE id=? AND role='admin' AND deleted_at IS NULL`, string(hash), security.Prefix, security.Fingerprint, userID)
	if err != nil {
		return security, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return security, errors.New("目标管理员不存在")
	}
	return security, nil
}

func (s *AccountStore) CheckPassword(ctx context.Context, password, secret string) PasswordCheckResult {
	digest := sha1.Sum([]byte(password))
	fullHash := strings.ToUpper(hex.EncodeToString(digest[:]))
	result := PasswordCheckResult{Prefix: fullHash[:5], Status: "ok"}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(fullHash))
		result.Fingerprint = strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.pwnedpasswords.com/range/"+result.Prefix, nil)
	if err != nil {
		result.Status = "unavailable"
		return result
	}
	request.Header.Set("User-Agent", "QVMConsole-Manager-PasswordCheck")
	response, err := s.client.Do(request)
	if err != nil {
		result.Status = "unavailable"
		return result
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		result.Status = "unavailable"
		return result
	}
	suffix := fullHash[5:]
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		result.Status = "unavailable"
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], suffix) {
			result.Status = "breached"
			result.Count, _ = strconv.Atoi(parts[1])
			break
		}
	}
	return result
}

func (s *AccountStore) backupDatabase(ctx context.Context, db *sql.DB, label string) error {
	dir := filepath.Join(s.cfg.VarDir, "backups", "accounts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.db", time.Now().Format("20060102-150405.000"), label))
	escaped := strings.ReplaceAll(filepath.ToSlash(path), "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return fmt.Errorf("创建数据库备份失败: %w", err)
	}
	entries, _ := os.ReadDir(dir)
	var files []os.DirEntry
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".db") {
			files = append(files, entry)
		}
	}
	if len(files) > 10 {
		for _, entry := range files[:len(files)-10] {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	return nil
}

type AccountError struct {
	Code    string
	Message string
}

func (e *AccountError) Error() string { return e.Message }
