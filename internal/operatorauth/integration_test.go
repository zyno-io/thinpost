package operatorauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/zyno-io/thinpost/internal/admin"
	"github.com/zyno-io/thinpost/internal/postgres"
)

func TestOperatorAPIWithJWKSAndPostgres(t *testing.T) {
	dsn := os.Getenv("THINPOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schemaName := "operator_test_" + hex.EncodeToString(random[:])
	if _, err = db.Exec("CREATE SCHEMA " + schemaName); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Exec("SET search_path TO public")
		_, _ = db.Exec("DROP SCHEMA " + schemaName + " CASCADE")
		_ = db.Close()
	}()
	if _, err = db.Exec("SET search_path TO " + schemaName); err != nil {
		t.Fatal(err)
	}
	if err = postgres.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	const testGeneration = "00000000000000000000000000000aaa"
	if _, err = db.Exec(`UPDATE runtime_gate SET generation=$1,mode='open' WHERE singleton`, testGeneration); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":"key-1","alg":"RS256","use":"sig","n":%q,"e":%q}]}`, n, e)
	}))
	defer jwks.Close()
	config := Config{Issuer: "https://issuer.example", Audience: "api://thinpost", JWKSURL: jwks.URL, RequiredScope: "admin", AdminRole: "Admin", Algorithms: []string{"RS256"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	auth, err := newWithClient(ctx, config, jwks.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": config.Issuer, "aud": config.Audience, "sub": "operator", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scp": "admin", "roles": []string{"Admin"}})
	token.Header["kid"] = "key-1"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	handler := &admin.Handler{Authenticator: auth, Store: &postgres.Repository{DB: db, Generation: testGeneration}}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		request.Header.Set("Authorization", "Bearer "+raw)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := call("POST", "/api/v1/apps", `{"name":"Payments"}`)
	if response.Code != 201 {
		t.Fatalf("create app: %d %s", response.Code, response.Body.String())
	}
	var app admin.App
	if err = json.Unmarshal(response.Body.Bytes(), &app); err != nil {
		t.Fatal(err)
	}
	response = call("POST", "/api/v1/apps/"+app.ID+"/keys", `{"name":"Primary"}`)
	if response.Code != 201 {
		t.Fatalf("issue: %d", response.Code)
	}
	var issued admin.IssuedKey
	if err = json.Unmarshal(response.Body.Bytes(), &issued); err != nil || issued.Secret == "" {
		t.Fatal("missing one-time key")
	}
	digest := sha256.Sum256([]byte(issued.Secret))
	repo := &postgres.Repository{DB: db, Generation: testGeneration}
	principal, err := repo.LookupKey(ctx, digest)
	if err != nil || principal.ID != app.ID {
		t.Fatal("issued key not scoped to app")
	}
	response = call("GET", "/api/v1/apps/"+app.ID+"/keys", "")
	if response.Code != 200 || bytes.Contains(response.Body.Bytes(), []byte(issued.Secret)) {
		t.Fatal("key listing exposed secret")
	}
	response = call("DELETE", "/api/v1/apps/"+app.ID+"/keys/"+issued.Key.ID, "")
	if response.Code != 204 {
		t.Fatal("revoke failed")
	}
	if _, err = repo.LookupKey(ctx, digest); err == nil {
		t.Fatal("revoked key authenticated")
	}
	var audit int
	if err = db.QueryRow(`SELECT count(*) FROM audit_events WHERE app_id=$1 AND actor_id='operator'`, app.ID).Scan(&audit); err != nil || audit != 3 {
		t.Fatal("audit records missing")
	}
}
