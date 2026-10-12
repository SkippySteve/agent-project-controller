package web

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOIDCVerifiedLoginAndRejection(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	var issuer string
	var idToken, challenge string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "fixture", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, "PKCE mismatch", 400)
			return
		}
		writeJSON(w, map[string]any{"access_token": "fixture-access", "token_type": "Bearer", "id_token": idToken})
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	issuer = server.URL
	auth, e := newOIDC(context.Background(), OIDCConfig{Issuer: issuer, ClientID: "client", ClientSecret: "secret", Origin: "https://projects.example.com", AllowedGroups: []string{"operators"}}, server.Client())
	if e != nil {
		t.Fatal(e)
	}
	sign := func(claims map[string]any) string {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture"}`))
		raw, _ := json.Marshal(claims)
		message := header + "." + base64.RawURLEncoding.EncodeToString(raw)
		hash := sha256.Sum256([]byte(message))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		if err != nil {
			t.Fatal(err)
		}
		return message + "." + base64.RawURLEncoding.EncodeToString(signature)
	}
	for _, test := range []struct {
		name   string
		modify func(map[string]any)
		want   int
	}{{"valid", func(map[string]any) {}, 303}, {"unauthorized group", func(c map[string]any) { c["groups"] = []string{"other"} }, 403}, {"wrong nonce", func(c map[string]any) { c["nonce"] = "other" }, 401}, {"wrong audience", func(c map[string]any) { c["aud"] = "another-client" }, 401}, {"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, 401}, {"wrong issuer", func(c map[string]any) { c["iss"] = "https://evil.example.com" }, 401}} {
		t.Run(test.name, func(t *testing.T) {
			login := httptest.NewRecorder()
			auth.Login(login, httptest.NewRequest("GET", "/auth/login", nil))
			location, _ := url.Parse(login.Header().Get("Location"))
			query := location.Query()
			if query.Get("code_challenge_method") != "S256" {
				t.Fatal("PKCE missing")
			}
			challenge = query.Get("code_challenge")
			claims := map[string]any{"iss": issuer, "aud": "client", "sub": "person", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": query.Get("nonce"), "groups": []string{"operators"}}
			test.modify(claims)
			idToken = sign(claims)
			callback := httptest.NewRequest("GET", "/auth/callback?code=fixture&state="+query.Get("state"), nil)
			callback.AddCookie(login.Result().Cookies()[0])
			result := httptest.NewRecorder()
			auth.Callback(result, callback)
			if result.Code != test.want {
				t.Fatalf("got %d: %s", result.Code, result.Body.String())
			}
			if test.want == 303 {
				var sessionCookie *http.Cookie
				for _, cookie := range result.Result().Cookies() {
					if cookie.Name == "__Host-apc-session" {
						sessionCookie = cookie
					}
				}
				if sessionCookie == nil || !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
					t.Fatal("unsafe session cookie")
				}
				request := httptest.NewRequest("GET", "/", nil)
				request.AddCookie(sessionCookie)
				subject, ok := auth.Subject(request)
				if !ok || !strings.HasSuffix(subject, "#person") {
					t.Fatal("session missing")
				}
				out := httptest.NewRecorder()
				auth.Logout(out, request)
				if _, ok = auth.Subject(request); ok {
					t.Fatal("logout did not invalidate session")
				}
			}
			replay := httptest.NewRecorder()
			auth.Callback(replay, callback)
			if replay.Code != 400 {
				t.Fatal("login transaction replay accepted")
			}
		})
	}
	result := httptest.NewRecorder()
	auth.Callback(result, httptest.NewRequest("GET", "/auth/callback?code=x&state=other", nil))
	if result.Code != 400 {
		t.Fatal("unbound login accepted")
	}
}
