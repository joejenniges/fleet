//go:build !ncruces

package store

// modernc.org/sqlite is pure Go (no cgo) and starts fast, which matters because
// hooks open the database on every tool call.
import _ "modernc.org/sqlite"

const driverName = "sqlite"

func dsn(path string) string { return "file:" + path }
