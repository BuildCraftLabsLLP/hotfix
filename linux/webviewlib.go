//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// libwebview.so — the webview C library (github.com/webview/webview) — is
// built by CI from pinned source against WebKitGTK 4.1 and dropped into
// webviewlib/ before `go build`, which embeds it here. A local build without
// it still compiles; the settings window then reports it is unavailable and
// the tray falls back to opening config.json.
//
//go:embed webviewlib
var webviewLibFS embed.FS

const webviewLibName = "libwebview.so"

var errNoWebviewLib = errors.New("this build has no embedded webview library")

// extractWebviewLib writes the embedded library to a private per-user
// directory and returns its path. The directory is named after the library's
// hash, so versions never overwrite a file another Hotfix has loaded, and an
// existing file is reused only if its contents match. Nothing is written to a
// shared location such as /tmp, where another user could plant a library.
func extractWebviewLib() (string, error) {
	lib, err := webviewLibFS.ReadFile("webviewlib/" + webviewLibName)
	if err != nil {
		return "", errNoWebviewLib
	}
	sum := sha256.Sum256(lib)

	base, err := privateDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "webview-"+hex.EncodeToString(sum[:8]))
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, webviewLibName)

	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, lib) {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, ".libwebview-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(lib); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o700); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// privateDir is $XDG_RUNTIME_DIR/hotfix (per-user, cleared at logout), or
// ~/.cache/hotfix where there's no runtime dir.
func privateDir() (string, error) {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && filepath.IsAbs(rt) {
		dir := filepath.Join(rt, "hotfix")
		if err := ensurePrivateDir(dir); err == nil {
			return dir, nil
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "hotfix")
	if err := ensurePrivateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// ensurePrivateDir creates dir (mode 0700) if needed and checks it is a real
// directory owned by the current user that nobody else can write to.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not a directory owned by this user", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}
