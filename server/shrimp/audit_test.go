package shrimp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditRejectsAmbiguousAuthorityBeforeStorage(t *testing.T) {
	secret := strings.Repeat("a", 32)
	handler := (&Handler{}).OperationalAuditHandler(secret)
	for _, values := range [][]string{nil, {"Bearer " + secret, "Bearer unrelated"}, {"Bearer unrelated", "Bearer " + secret}} {
		r := httptest.NewRequest(http.MethodGet, "https://pilot.example/__shrimp/audit", nil)
		for _, value := range values {
			r.Header.Add("Authorization", value)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}
}
