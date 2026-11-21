package store

import (
	"testing"

	"example.com/demo/pkg/db"
)

func TestGet(t *testing.T) {
	if Get() != db.Query() {
		t.Fatal("store disagrees with db")
	}
}
