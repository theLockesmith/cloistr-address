package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/auth"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/config"
	"git.aegis-hq.xyz/coldforge/cloistr-me/internal/storage"
)

// A signed-in caller's availability check must be answered for THEM: a name
// reserved for their pubkey is available to them and to nobody else, so the
// lookup has to carry who is asking.
func TestCheckUsernameAvailability_AsksForTheCaller(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const caller = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	gin.SetMode(gin.TestMode)
	h := &Handler{cfg: &config.Config{}, store: storage.NewWithDB(db)}
	r := gin.New()
	r.GET("/api/v1/addresses/check/:username",
		func(c *gin.Context) { c.Set(auth.PubkeyContextKey, caller); c.Next() },
		h.checkUsernameAvailability)

	mock.ExpectQuery("is_username_available").
		WithArgs("heldforme", caller).
		WillReturnRows(sqlmock.NewRows([]string{"available"}).AddRow(false))
	mock.ExpectQuery("reserved_usernames").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/addresses/check/heldforme", nil))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("availability was not looked up for the caller: %v", err)
	}
}
