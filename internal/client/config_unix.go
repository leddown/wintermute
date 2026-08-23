//go:build !windows

package client

import (
	"os"
	"path/filepath"
)

// exampleRoot is the placeholder written into a starter config.
func exampleRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/srv/files"
	}
	return filepath.Join(home, "Documents")
}
