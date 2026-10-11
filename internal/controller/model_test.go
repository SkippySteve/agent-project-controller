package controller

import "testing"

func TestRejectRefreshMaterial(t *testing.T) {
	if e := RejectRefreshMaterial(map[string][]byte{"access-token": []byte("x")}); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"refresh-token", "refresh_token", "refreshToken"} {
		if e := RejectRefreshMaterial(map[string][]byte{key: []byte("x")}); e == nil {
			t.Fatal("accepted", key)
		}
	}
}
