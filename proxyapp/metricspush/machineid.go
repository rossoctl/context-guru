package metricspush

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// MachineID returns the random ID of this install (R43). It is made once and kept in path, so it
// survives a restart. It is random on purpose: it holds no host name, no user name and no address,
// so it names an install and nobody.
//
// If path is empty or cannot be written, MachineID returns a fresh ID for this run and the error. The
// push still works. The same machine then shows up as a new `machine` value after each restart, which
// is a small, bounded cost.
func MachineID(path string) (string, error) {
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(b)); validID(id) {
				return id, nil
			}
		}
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	if path == "" {
		return id, errors.New("metricspush: no path for the machine ID; it lasts for this run only")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return id, err
	}
	// 0600: the ID is not secret, but nobody else needs to read it.
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return id, err
	}
	return id, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validID accepts what newID makes: 32 lowercase hex digits. A file with anything else is replaced.
func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
