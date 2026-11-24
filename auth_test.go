package gsheets_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/viant/gsheets"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// A user token can expire after Open/Connect. Refresh uses that connector's
// client configuration even after the original Connect context was canceled.
func TestPerUserOAuth(t *testing.T) {
	var refreshes atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "fixture-refresh" {
			http.Error(w, "bad refresh", 400)
			return
		}
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fixture-access","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-access" {
			http.Error(w, "wrong token", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"spreadsheetId":"fixture","sheets":[]}`)
	}))
	defer api.Close()
	write := func(name string, v any) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := gsheets.Config{SpreadsheetID: "fixture", OAuth2ClientURL: write("client.json", oauth2.Config{ClientID: "fixture", ClientSecret: "fixture", Endpoint: oauth2.Endpoint{TokenURL: tokenServer.URL}}), OAuth2TokenURL: write("token.json", oauth2.Token{RefreshToken: "fixture-refresh"})}
	conn, err := gsheets.NewConnector(cfg, option.WithEndpoint(api.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = db.Ping(); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("expected one refresh, got %d", refreshes.Load())
	}
}
