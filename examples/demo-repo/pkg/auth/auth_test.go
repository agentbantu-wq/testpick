package auth

import "testing"

func TestLogin(t *testing.T) {
	if !Login() {
		t.Fatal("login failed")
	}
}
