package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Refresh refuses a request without both a token and an identifier
// before it looks anything up.
func TestRefreshNeedsTokenAndIdentifier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewLicenseHandler(nil)
	for _, body := range []string{
		`{}`,
		`{"token":"t"}`,
		`{"identifier":"pc"}`,
		`{"token":"t","identifier":"   "}`,
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/license/refresh", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.Refresh(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, w.Code)
		}
	}
}
