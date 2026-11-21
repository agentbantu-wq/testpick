package main

import (
	"testing"

	"example.com/demo/pkg/store"
)

func TestMain(t *testing.T) { _ = store.Get() }
