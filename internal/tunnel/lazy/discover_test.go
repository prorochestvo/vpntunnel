package lazy_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vpntunnel/internal/publicerror"
	"vpntunnel/internal/tunnel/lazy"
)

func TestDiscoverConfigs(t *testing.T) {
	t.Parallel()

	t.Run("returns sorted absolute paths for top-level .conf files", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// create in non-sorted order to verify the returned slice is always sorted.
		for _, name := range []string{"charlie.conf", "alpha.conf", "bravo.conf"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(""), 0o600))
		}

		got, err := lazy.DiscoverConfigs(dir)
		require.NoError(t, err)
		require.Len(t, got, 3)

		// all paths must be absolute.
		for _, p := range got {
			assert.True(t, filepath.IsAbs(p), "expected absolute path, got %q", p)
		}

		// must be sorted ascending.
		assert.Equal(t, filepath.Join(dir, "alpha.conf"), got[0])
		assert.Equal(t, filepath.Join(dir, "bravo.conf"), got[1])
		assert.Equal(t, filepath.Join(dir, "charlie.conf"), got[2])
	})

	t.Run("ignores .gitkeep, dotfiles, and non-.conf files", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitkeep"), []byte(""), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden.conf"), []byte(""), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(""), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.conf"), []byte(""), 0o600))

		got, err := lazy.DiscoverConfigs(dir)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, filepath.Join(dir, "a.conf"), got[0])
	})

	t.Run("ignores subdirectories and does not recurse", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// create a subdirectory containing a .conf file.
		subDir := filepath.Join(dir, "sub")
		require.NoError(t, os.MkdirAll(subDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(subDir, "nested.conf"), []byte(""), 0o600))

		got, err := lazy.DiscoverConfigs(dir)
		require.NoError(t, err)
		// the nested.conf must not appear.
		assert.Empty(t, got)
	})

	t.Run("ignores a directory whose name ends in .conf", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// create a directory named "x.conf" — must be ignored.
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "x.conf"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.conf"), []byte(""), 0o600))

		got, err := lazy.DiscoverConfigs(dir)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, filepath.Join(dir, "real.conf"), got[0])
	})

	t.Run("empty but present directory returns nil slice and no error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		got, err := lazy.DiscoverConfigs(dir)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("missing directory returns a PublicError naming the path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		missing := filepath.Join(dir, "nonexistent-tunnels")

		_, err := lazy.DiscoverConfigs(missing)
		require.Error(t, err)

		var pe *publicerror.Error
		require.True(t, errors.As(err, &pe), "expected *publicerror.Error, got: %T %v", err, err)
		assert.Contains(t, pe.Details(), missing)
	})

	t.Run("symlink to a .conf file is excluded", func(t *testing.T) {
		t.Parallel()
		// place the real .conf in a separate dir so it is not itself discovered.
		srcDir := t.TempDir()
		scanDir := t.TempDir()

		realConf := filepath.Join(srcDir, "real.conf")
		require.NoError(t, os.WriteFile(realConf, []byte(""), 0o600))

		// a regular .conf in the scan dir — must be returned.
		regularConf := filepath.Join(scanDir, "regular.conf")
		require.NoError(t, os.WriteFile(regularConf, []byte(""), 0o600))

		// a symlink pointing at the real .conf — must be excluded per the
		// documented "regular files only" contract.
		linkConf := filepath.Join(scanDir, "linked.conf")
		require.NoError(t, os.Symlink(realConf, linkConf))

		got, err := lazy.DiscoverConfigs(scanDir)
		require.NoError(t, err)
		require.Len(t, got, 1, "expected only the regular .conf, got %v", got)
		assert.Equal(t, regularConf, got[0])
	})

	t.Run("unreadable directory returns a plain error not a PublicError", func(t *testing.T) {
		t.Parallel()
		if os.Getuid() == 0 {
			t.Skip("running as root: chmod 0o000 has no effect")
		}

		dir := t.TempDir()
		tunnelsDir := filepath.Join(dir, "tunnels")
		require.NoError(t, os.Mkdir(tunnelsDir, 0o700))
		require.NoError(t, os.Chmod(tunnelsDir, 0o000))
		t.Cleanup(func() { _ = os.Chmod(tunnelsDir, 0o700) })

		_, err := lazy.DiscoverConfigs(tunnelsDir)
		require.Error(t, err)

		var pe *publicerror.Error
		assert.False(t, errors.As(err, &pe),
			"expected a plain wrapped error, got *publicerror.Error: %v", err)
	})
}
