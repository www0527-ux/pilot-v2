package security

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"shanyangcode.com/pilot/internal/identity"
)

func TestDeviceAccountCookieIdentityAndOriginChecks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	s, err := NewAccountStore(db)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/api/v1/account/session", s.Session)
	r.GET("/api/v1/private", s.RequireAccount(), func(c *gin.Context) {
		c.JSON(200, map[string]string{"owner": identity.Owner(c.Request.Context()), "user_id": c.Query("user_id")})
	})
	r.POST("/api/v1/private", s.RequireAccount(), func(c *gin.Context) { c.Status(200) })
	create := func() (*http.Cookie, string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "http://pilot.local/api/v1/account/session", nil)
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("create account: %d", w.Code)
		}
		cookie := w.Result().Cookies()[0]
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
			t.Fatal("account cookie lacks browser restrictions")
		}
		var body struct {
			Data struct {
				ID string `json:"user_id"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &body)
		return cookie, body.Data.ID
	}
	cookieA, ownerA := create()
	_, ownerB := create()
	if ownerA == ownerB {
		t.Fatal("accounts share identity")
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://pilot.local/api/v1/private?user_id="+ownerB, nil)
	req.AddCookie(cookieA)
	r.ServeHTTP(w, req)
	var actual map[string]string
	json.Unmarshal(w.Body.Bytes(), &actual)
	if actual["owner"] != ownerA || actual["user_id"] != ownerA {
		t.Fatal("caller could choose a different owner")
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://pilot.local/api/v1/private?user_id="+ownerA, nil)
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("user_id treated as a credential")
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://pilot.local/api/v1/private", nil)
	req.AddCookie(cookieA)
	req.Header.Set("Origin", "https://attacker.example")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("cross-origin mutation accepted")
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://pilot.local/api/v1/account/session", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("cross-site account bootstrap accepted")
	}
	// Restarting the server-side account store retains the same identity.
	s, err = NewAccountStore(db)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://pilot.local/", nil)
	c.Request.AddCookie(cookieA)
	account, err := s.lookup(c)
	if err != nil || account.ID != ownerA {
		t.Fatal("account identity lost after restart")
	}
}
