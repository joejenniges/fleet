//go:build ncruces

package store

// Alternative pure-Go driver (WASM via wazero), selected with `-tags ncruces`.
// Slower process start than modernc; kept as a fallback for environments where
// modernc.org cannot be fetched.
import (
	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

const driverName = "sqlite3"

func dsn(path string) string { return "file:" + path }
