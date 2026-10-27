package policy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/joejenniges/fleet/internal/paths"
)

// Cache keeps the last valid copy of each policy file so a broken edit never
// leaves fleet without a policy. store.Store satisfies it.
type Cache interface {
	MetaGet(key string) (string, bool)
	MetaSet(key, value string) error
}

type Level struct {
	Name   string `json:"level"`
	Path   string `json:"file"`
	Exists bool   `json:"exists"`
	// Error is set when the file failed to parse/validate; File then holds the
	// last good cached copy (or is empty).
	Error     string `json:"error,omitempty"`
	FromCache bool   `json:"from_cache,omitempty"`
	File      File   `json:"-"`
}

// Set is every policy level that applies at one location.
type Set struct {
	Levels   []*Level
	RepoRoot string
}

// LevelPath returns the file for an editable level.
func LevelPath(level, repoRoot string) (string, error) {
	switch level {
	case LevelGlobal:
		return filepath.Join(paths.GlobalConfigDir(), "policy.toml"), nil
	case LevelUser:
		d, err := paths.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "policy.toml"), nil
	case LevelRepo:
		if repoRoot == "" {
			return "", errors.New("not inside a git repository (repo level needs one)")
		}
		return paths.RepoPolicyPath(repoRoot), nil
	}
	return "", fmt.Errorf("unknown level %q (global|user|repo)", level)
}

// Load reads builtin, global, user and (if repoRoot != "") repo policy.
func Load(repoRoot string, cache Cache) *Set {
	s := &Set{RepoRoot: repoRoot}
	b := Builtin()
	_ = b.Validate()
	s.Levels = append(s.Levels, &Level{Name: LevelBuiltin, Path: "(builtin)", Exists: true, File: b})
	for _, name := range []string{LevelGlobal, LevelUser, LevelRepo} {
		if name == LevelRepo && repoRoot == "" {
			continue
		}
		p, err := LevelPath(name, repoRoot)
		if err != nil {
			continue
		}
		s.Levels = append(s.Levels, loadLevel(name, p, cache))
	}
	return s
}

func loadLevel(name, path string, cache Cache) *Level {
	lv := &Level{Name: name, Path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return lv
	}
	lv.Exists = true
	if err == nil {
		var f File
		if err = toml.Unmarshal(raw, &f); err == nil {
			if err = f.Validate(); err == nil {
				lv.File = f
				if cache != nil {
					_ = cache.MetaSet(cacheKey(path), string(raw))
				}
				return lv
			}
		}
	}
	lv.Error = err.Error()
	if cache != nil {
		if good, ok := cache.MetaGet(cacheKey(path)); ok {
			var f File
			if toml.Unmarshal([]byte(good), &f) == nil && f.Validate() == nil {
				lv.File, lv.FromCache = f, true
			}
		}
	}
	return lv
}

func cacheKey(path string) string { return "policy-cache:" + paths.Canon(path) }

// ReadFile loads one policy file strictly (no cache fallback). Missing = empty File.
func ReadFile(path string) (File, error) {
	var f File
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := toml.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, f.Validate()
}

const fileHeader = "# Managed by fleet. Edit with `fleet policy add/remove/set-default`, not by hand.\n" +
	"# Direct edits are blocked for agents; invalid files fall back to the last good copy.\n\n"

// WriteFile validates and atomically writes a policy file.
func WriteFile(path string, f File) error {
	if err := f.Validate(); err != nil {
		return err
	}
	b, err := toml.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".policy-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(fileHeader + string(b)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
