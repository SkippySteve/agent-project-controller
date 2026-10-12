package controller

import (
	"strconv"
	"testing"
	"time"
)

func validModelAccess() map[string][]byte {
	return map[string][]byte{"access-token": []byte("fixture-access"), "chatgpt-account-id": []byte("fixture-account"), "expires-at": []byte(strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))}
}

func TestRejectRefreshMaterial(t *testing.T) {
	if err := RejectRefreshMaterial(validModelAccess()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"refresh-token", "refresh_token", "refreshToken", "auth.json", "tokens", "id-token"} {
		data := validModelAccess()
		data[key] = []byte(`{"refresh_token":"do-not-deliver"}`)
		if err := RejectRefreshMaterial(data); err == nil {
			t.Fatal("accepted credential container", key)
		}
	}
	for _, expiry := range []string{"", "1", "NaN", "+Inf", "-Inf", "1e9999"} {
		data := validModelAccess()
		data["expires-at"] = []byte(expiry)
		if err := RejectRefreshMaterial(data); err == nil {
			t.Fatal("accepted invalid expiry", expiry)
		}
	}
	for _, token := range []string{"", "x\ny", `{"refresh_token":"nested"}`} {
		data := validModelAccess()
		data["access-token"] = []byte(token)
		if err := RejectRefreshMaterial(data); err == nil {
			t.Fatal("accepted malformed access material")
		}
	}
}
