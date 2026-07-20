package tunnelpool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vpntunnel/internal/publicerror"
)

// DiscoverConfigs lists the top-level *.conf files in tunnelsDir and returns
// their absolute paths sorted ascending. Only regular files (not symlinks or
// directories) with a ".conf" suffix are returned. Dotfiles (names beginning
// with "."), including ".gitkeep", are skipped. Subdirectories are not
// recursed into.
//
// If tunnelsDir does not exist, DiscoverConfigs returns a *publicerror.Error
// naming the path with an operator-actionable message. Other os.ReadDir errors
// (permission denied, not a directory) are returned as plain wrapped errors.
//
// An empty-but-present tunnelsDir returns (nil, nil); the empty-set error is
// the responsibility of the caller (NewEligibleSet) so the missing-dir and
// empty-after-country-filter cases share a single consistent error path.
func DiscoverConfigs(tunnelsDir string) ([]string, error) {
	entries, err := os.ReadDir(tunnelsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, publicerror.New(fmt.Sprintf(
				"config.upstream: tunnels directory %s not found; "+
					"create it and drop your wg-quick .conf files there",
				tunnelsDir,
			))
		}
		return nil, fmt.Errorf("tunnelpool: read tunnels dir %s: %w", tunnelsDir, err)
	}

	var paths []string
	for _, entry := range entries {
		name := entry.Name()

		// skip dotfiles (includes .gitkeep and any .hidden.conf).
		if strings.HasPrefix(name, ".") {
			continue
		}

		// skip directories and non-regular files (symlinks, devices, etc.).
		if !entry.Type().IsRegular() {
			continue
		}

		// skip files whose name does not end in ".conf".
		if !strings.HasSuffix(name, ".conf") {
			continue
		}

		abs, err := filepath.Abs(filepath.Join(tunnelsDir, name))
		if err != nil {
			return nil, fmt.Errorf("tunnelpool: resolve config path %q: %w", name, err)
		}
		paths = append(paths, abs)
	}

	sort.Strings(paths)
	return paths, nil
}
