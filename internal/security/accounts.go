package security

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"shanyangcode.com/pilot/internal/httpapi"
	"shanyangcode.com/pilot/internal/identity"
)

// A minimal device account for a self-hosted console, not a full login/IAM
// system. The opaque HttpOnly cookie is a credential; user_id is only a label.
type ConsoleAccount struct {
	ID             string `gorm:"primaryKey;size:128"`
	CredentialHash string `gorm:"size:64;uniqueIndex;not null"`
	CreatedAt      time.Time
}
type AccountStore struct{ db *gorm.DB }

func NewAccountStore(db *gorm.DB) (*AccountStore, error) {
	if err := db.AutoMigrate(&ConsoleAccount{}); err != nil {
		return nil, err
	}
	return &AccountStore{db: db}, nil
}

const accountCookie = "pilot_account"

func hashCredential(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func (s *AccountStore) lookup(c *gin.Context) (ConsoleAccount, error) {
	token, err := c.Cookie(accountCookie)
	if err != nil || len(token) != 64 {
		return ConsoleAccount{}, gorm.ErrRecordNotFound
	}
	var account ConsoleAccount
	err = s.db.WithContext(c.Request.Context()).Where("credential_hash = ?", hashCredential(token)).First(&account).Error
	return account, err
}
func sameOrigin(c *gin.Context) bool {
	if c.GetHeader("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := c.GetHeader("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, c.Request.Host) && (u.Scheme == "http" || u.Scheme == "https")
}
func (s *AccountStore) Session(c *gin.Context) {
	if !sameOrigin(c) {
		httpapi.Fail(c, httpapi.Unauthenticated("请从同域控制台创建账号会话"))
		return
	}
	account, err := s.lookup(c)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		httpapi.Fail(c, httpapi.DependencyUnavailable("账号读取失败"))
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			httpapi.Fail(c, httpapi.Internal("账号创建失败"))
			return
		}
		token := hex.EncodeToString(raw[:])
		account = ConsoleAccount{ID: "account_" + uuid.NewString(), CredentialHash: hashCredential(token)}
		if err := s.db.WithContext(c.Request.Context()).Create(&account).Error; err != nil {
			httpapi.Fail(c, httpapi.DependencyUnavailable("账号创建失败"))
			return
		}
		http.SetCookie(c.Writer, &http.Cookie{Name: accountCookie, Value: token, Path: "/api/v1", HttpOnly: true, Secure: c.Request.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 60 * 60})
	}
	c.Header("Cache-Control", "no-store")
	httpapi.OK(c, "账号会话已准备", map[string]any{"user_id": account.ID, "kind": "device"})
}
func (s *AccountStore) RequireAccount() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && !sameOrigin(c) {
			httpapi.Fail(c, httpapi.Unauthenticated("请求来源不匹配"))
			c.Abort()
			return
		}
		account, err := s.lookup(c)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				httpapi.Fail(c, httpapi.Unauthenticated("请先打开控制台建立账号会话"))
			} else {
				httpapi.Fail(c, httpapi.DependencyUnavailable("账号读取失败"))
			}
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(identity.WithOwner(c.Request.Context(), account.ID))
		// GET/cancel/history never trust a caller-provided ownership label.
		q := c.Request.URL.Query()
		q.Set("user_id", account.ID)
		c.Request.URL.RawQuery = q.Encode()
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}
