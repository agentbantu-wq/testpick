package store

import "example.com/demo/pkg/db"

// Get serves one row through the store layer.
func Get() string { return db.Query() }
