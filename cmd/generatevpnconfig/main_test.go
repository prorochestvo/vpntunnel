package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/infrastructure/wireguard/wgconf"
)

// discardLog drops wgconf parser warnings so test output stays clean and does
// not depend on future fixture content (matches what production passes).
var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// ttyCfg returns a runConfig with TTY forced to true, using the supplied
// reader/writer pair and the given temp dir.
func ttyCfg(in *bytes.Buffer, out *bytes.Buffer, dir string) runConfig {
	return runConfig{
		in:    in,
		out:   out,
		dir:   dir,
		force: false,
		isTTY: func() bool { return true },
	}
}

// genPubKey generates a random WireGuard public key and returns its base64
// string. Used only for peer-key test inputs.
func genPubKey(tb testing.TB) string {
	tb.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	require.NoError(tb, err)
	return k.PublicKey().String()
}

// validInput assembles a complete multi-line stdin string for a successful run
// using the supplied conf name. It selects manual mode (choice "1").
func validInput(name, pubKey string) string {
	return strings.Join([]string{
		"1", // mode: manual
		name,
		"10.66.1.2/32",
		pubKey,
		"185.1.2.3:51820",
		"", // DNS — accept default
		"", // AllowedIPs — accept default
		""}, "\n")
}

func TestRun(t *testing.T) {
	t.Parallel()

	t.Run("non-TTY returns error immediately without reading input", func(t *testing.T) {
		t.Parallel()
		// preload input so the assertion below is meaningful: if run() reads
		// stdin before the TTY check, the buffer length would shrink.
		in := bytes.NewBufferString("should-never-be-read\n")
		initialLen := in.Len()
		out := &bytes.Buffer{}
		cfg := runConfig{
			in:    in,
			out:   out,
			dir:   t.TempDir(),
			force: false,
			isTTY: func() bool { return false },
		}
		err := run(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "interactive terminal required")
		// nothing should have been read from in
		assert.Equal(t, initialLen, in.Len(), "no input should be consumed before TTY check")
		assert.Equal(t, 0, out.Len(), "nothing should be written to out before TTY check")
	})

	t.Run("happy path writes valid .conf and prints snippet", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pubKey := genPubKey(t)
		in := bytes.NewBufferString(validInput("my-tunnel", pubKey))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		// file must exist at the expected path
		confPath := filepath.Join(dir, "my-tunnel.conf")
		fi, err := os.Stat(confPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "file mode must be 0600")

		// round-trip: the written file must parse cleanly
		parsed, err := wgconf.Parse(confPath, discardLog)
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", parsed.Interface.Addresses[0].String())
		assert.Equal(t, pubKey, parsed.Peer.PublicKey)
		assert.Equal(t, "185.1.2.3:51820", parsed.Peer.Endpoint)
		assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.64.0.1")}, parsed.Interface.DNS)
		require.Len(t, parsed.Peer.AllowedIPs, 2)
		assert.Equal(t, netip.MustParsePrefix("0.0.0.0/0"), parsed.Peer.AllowedIPs[0])
		assert.Equal(t, netip.MustParsePrefix("::/0"), parsed.Peer.AllowedIPs[1])

		// output describes auto-discovery, not a manual proxy.json snippet.
		output := out.String()
		assert.Contains(t, output, "configs/tunnels/")
		assert.Contains(t, output, "auto-discovers")
	})

	t.Run("private key is never printed to output", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pubKey := genPubKey(t)
		in := bytes.NewBufferString(validInput("key-leak-check", pubKey))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		// read the written file to get the private key string
		confPath := filepath.Join(dir, "key-leak-check.conf")
		raw, err := os.ReadFile(confPath)
		require.NoError(t, err)

		// extract the PrivateKey line from the written conf
		var privateKey string
		for _, line := range strings.Split(string(raw), "\n") {
			if after, found := strings.CutPrefix(line, "PrivateKey = "); found {
				privateKey = strings.TrimSpace(after)
				break
			}
		}
		require.NotEmpty(t, privateKey, "written .conf must contain a PrivateKey line")

		// the private key must NOT appear anywhere in the terminal output
		assert.NotContains(t, out.String(), privateKey,
			"private key must never appear in terminal output")
	})

	t.Run("existing file without -force returns error and leaves file unchanged", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		confPath := filepath.Join(dir, "existing.conf")
		original := []byte("original content")
		require.NoError(t, os.WriteFile(confPath, original, 0o600))

		pubKey := genPubKey(t)
		in := bytes.NewBufferString(validInput("existing", pubKey))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")

		// original file must be untouched
		got, readErr := os.ReadFile(confPath)
		require.NoError(t, readErr)
		assert.Equal(t, original, got, "original file must be unchanged")
	})

	t.Run("existing file with -force overwrites", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		confPath := filepath.Join(dir, "overwrite-me.conf")
		require.NoError(t, os.WriteFile(confPath, []byte("old"), 0o600))

		pubKey := genPubKey(t)
		in := bytes.NewBufferString(validInput("overwrite-me", pubKey))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)
		cfg.force = true

		err := run(cfg)
		require.NoError(t, err)

		// confirm the file was replaced with a valid .conf
		parsed, err := wgconf.Parse(confPath, discardLog)
		require.NoError(t, err)
		assert.Equal(t, pubKey, parsed.Peer.PublicKey)
	})

	t.Run("missing dir is created with mode 0700", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "subdir", "tunnels")

		pubKey := genPubKey(t)
		in := bytes.NewBufferString(validInput("mkdir-test", pubKey))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm()&os.FileMode(0o777))
	})

	t.Run("EOF on required field returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// provide mode + name, then EOF before address
		in := bytes.NewBufferString("1\nmy-tunnel\n")
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EOF")
	})

	t.Run("custom DNS is used when provided", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pubKey := genPubKey(t)
		in := bytes.NewBufferString(strings.Join([]string{
			"1", // mode: manual
			"custom-dns",
			"10.66.1.2/32",
			pubKey,
			"185.1.2.3:51820",
			"1.1.1.1", // custom DNS
			"",        // AllowedIPs default
			"",
		}, "\n"))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		parsed, err := wgconf.Parse(filepath.Join(dir, "custom-dns.conf"), discardLog)
		require.NoError(t, err)
		require.Len(t, parsed.Interface.DNS, 1)
		assert.Equal(t, netip.MustParseAddr("1.1.1.1"), parsed.Interface.DNS[0])
	})

	t.Run("custom AllowedIPs is used when provided", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pubKey := genPubKey(t)
		in := bytes.NewBufferString(strings.Join([]string{
			"1", // mode: manual
			"custom-aips",
			"10.66.1.2/32",
			pubKey,
			"185.1.2.3:51820",
			"",           // DNS default
			"10.0.0.0/8", // custom AllowedIPs
			"",
		}, "\n"))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		parsed, err := wgconf.Parse(filepath.Join(dir, "custom-aips.conf"), discardLog)
		require.NoError(t, err)
		require.Len(t, parsed.Peer.AllowedIPs, 1)
		assert.Equal(t, netip.MustParsePrefix("10.0.0.0/8"), parsed.Peer.AllowedIPs[0])
	})

	t.Run("zip mode writes all entries and prints snippet", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		peerKey := genPubKey(t)
		zipPath := buildTestZip(t, []zipEntry{
			{
				name: "se-001.conf",
				body: mullvadConf("10.66.1.1/32", peerKey, "185.1.2.3:51820"),
			},
			{
				name: "de-001.conf",
				body: mullvadConf("10.66.1.1/32", peerKey, "185.2.3.4:51820"),
			},
		})

		// drive the full flow through run(): mode=2, blank prefix, then the zip
		// path. run() generates its own key internally, so we read it back from
		// a written file to assert it never leaked to the terminal.
		in := bytes.NewBufferString("2\n\n" + zipPath + "\n")
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		// both files must exist and round-trip through the parser.
		for _, n := range []string{"se-001", "de-001"} {
			path := filepath.Join(dir, n+".conf")
			assert.FileExists(t, path)
			_, parseErr := wgconf.Parse(path, discardLog)
			require.NoError(t, parseErr, "written conf must round-trip: %s", n)
		}

		output := out.String()

		// the warning block from printZipInstructions must have appeared.
		assert.Contains(t, output, "MUST have been generated for the public key above")
		// output describes auto-discovery, not a manual proxy.json snippet.
		assert.Contains(t, output, "configs/tunnels/")
		assert.Contains(t, output, "auto-discovers")

		// read the injected private key back from a written file and assert it
		// never appears anywhere in the terminal output.
		raw, readErr := os.ReadFile(filepath.Join(dir, "se-001.conf"))
		require.NoError(t, readErr)
		var privKey string
		for _, line := range strings.Split(string(raw), "\n") {
			if after, ok := strings.CutPrefix(line, "PrivateKey = "); ok {
				privKey = strings.TrimSpace(after)
				break
			}
		}
		require.NotEmpty(t, privKey, "written .conf must contain a PrivateKey line")
		assert.NotContains(t, output, privKey, "private key must never appear in terminal output")
	})

	t.Run("zip mode applies the interactive prefix to written names", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		peerKey := genPubKey(t)
		zipPath := buildTestZip(t, []zipEntry{
			{name: "al-tia-wg-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "185.1.2.3:51820")},
		})

		// mode=2, prefix=mullvad, then the zip path.
		in := bytes.NewBufferString("2\nmullvad\n" + zipPath + "\n")
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)

		assert.FileExists(t, filepath.Join(dir, "mullvad-al-tia-wg-001.conf"))
		assert.Contains(t, out.String(), "configs/tunnels/")
		assert.Contains(t, out.String(), "auto-discovers")
	})

	t.Run("zip mode: invalid mode input re-prompts then accepts", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		pubKey := genPubKey(t)
		// feed "3" (invalid), then "1" (manual), then valid manual fields.
		in := bytes.NewBufferString(strings.Join([]string{
			"3", // invalid
			"1", // manual
			"retry-tunnel",
			"10.66.1.2/32",
			pubKey,
			"185.1.2.3:51820",
			"",
			"",
			"",
		}, "\n"))
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.NoError(t, err)
		assert.Contains(t, out.String(), "error: enter 1 or 2")
		assert.FileExists(t, filepath.Join(dir, "retry-tunnel.conf"))
	})

	t.Run("zip mode: EOF before zip path returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// mode=2, then immediate EOF
		in := bytes.NewBufferString("2\n")
		out := &bytes.Buffer{}
		cfg := ttyCfg(in, out, dir)

		err := run(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EOF")
	})
}

func TestValidateName(t *testing.T) {
	t.Parallel()

	t.Run("valid names are accepted", func(t *testing.T) {
		t.Parallel()
		cases := []string{
			"mullvad-se-sto-wg-001",
			"tunnel1",
			"my.tunnel",
			"wg_001",
			"A",
		}
		for _, name := range cases {
			name := name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assert.NoError(t, validateName(name))
			})
		}
	})

	t.Run("empty string is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName(""))
	})

	t.Run("dot is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName("."))
	})

	t.Run("double dot is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName(".."))
	})

	t.Run("leading dot is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName(".hidden"))
	})

	t.Run("slash is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName("foo/bar"))
	})

	t.Run("backslash is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName(`foo\bar`))
	})

	t.Run("space is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateName("foo bar"))
	})
}

func TestValidateAddress(t *testing.T) {
	t.Parallel()

	t.Run("valid single-host IPv4 CIDR is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateAddress("10.66.1.2/32"))
	})

	t.Run("valid single-host IPv6 CIDR is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateAddress("fc00::5/128"))
	})

	t.Run("bare IPv4 host is rejected (validateAddress expects normalized CIDR)", func(t *testing.T) {
		// validateAddress operates on an already-normalized CIDR string.
		// promptAddress calls normalizeAddress first; bare hosts never reach
		// validateAddress in the prompt path.
		t.Parallel()
		assert.Error(t, validateAddress("10.66.1.2"))
	})

	t.Run("non-host-prefix CIDR is rejected", func(t *testing.T) {
		t.Parallel()
		// 10.0.0.0/8 is a network prefix, not a host address
		assert.Error(t, validateAddress("10.0.0.0/8"))
	})

	t.Run("empty is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateAddress(""))
	})

	t.Run("garbage is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateAddress("not-an-ip"))
	})
}

func TestValidatePeerPublicKey(t *testing.T) {
	t.Parallel()

	t.Run("valid WireGuard public key is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validatePeerPublicKey(genPubKey(t)))
	})

	t.Run("empty string is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validatePeerPublicKey(""))
	})

	t.Run("garbage base64 is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validatePeerPublicKey("not-a-valid-key===="))
	})
}

func TestValidateEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("valid IPv4 endpoint is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateEndpoint("185.1.2.3:51820"))
	})

	t.Run("valid IPv6 endpoint with brackets is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateEndpoint("[2001:db8::1]:51820"))
	})

	t.Run("valid hostname endpoint is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateEndpoint("se-sto-wg-001.mullvad.net:51820"))
	})

	t.Run("missing port separator is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateEndpoint("185.1.2.3"))
	})

	t.Run("port 0 is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateEndpoint("185.1.2.3:0"))
	})

	t.Run("port 65536 is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateEndpoint("185.1.2.3:65536"))
	})

	t.Run("non-numeric port is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateEndpoint("185.1.2.3:abc"))
	})

	t.Run("empty host is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateEndpoint(":51820"))
	})
}

func TestValidateDNS(t *testing.T) {
	t.Parallel()

	t.Run("valid IPv4 DNS is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateDNS("10.64.0.1"))
	})

	t.Run("valid IPv6 DNS is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateDNS("2001:db8::1"))
	})

	t.Run("empty string is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateDNS(""))
	})

	t.Run("CIDR notation is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateDNS("10.64.0.1/32"))
	})
}

func TestValidateAllowedIPs(t *testing.T) {
	t.Parallel()

	t.Run("valid single CIDR is accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateAllowedIPs("0.0.0.0/0"))
	})

	t.Run("valid comma-separated CIDRs are accepted", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, validateAllowedIPs("0.0.0.0/0, ::/0"))
	})

	t.Run("invalid CIDR is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateAllowedIPs("not-a-cidr"))
	})

	t.Run("bare IP without prefix length is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateAllowedIPs("10.0.0.1"))
	})

	t.Run("comma-only input yielding zero CIDRs is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateAllowedIPs(","))
		assert.Error(t, validateAllowedIPs(" , "))
	})
}

func TestPromptRequired(t *testing.T) {
	t.Parallel()

	t.Run("accepts first valid input", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("good-name\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptRequired(out, sc, "Name", validateName)
		require.NoError(t, err)
		assert.Equal(t, "good-name", got)
	})

	t.Run("re-prompts on invalid input then accepts valid", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("bad name!\ngood-name\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptRequired(out, sc, "Name", validateName)
		require.NoError(t, err)
		assert.Equal(t, "good-name", got)
		assert.Contains(t, out.String(), "error:")
	})

	t.Run("re-prompts on empty input then accepts valid", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("\ngood-name\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptRequired(out, sc, "Name", validateName)
		require.NoError(t, err)
		assert.Equal(t, "good-name", got)
	})

	t.Run("EOF on first scan returns error", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		_, err := promptRequired(out, sc, "Name", validateName)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EOF")
	})

	t.Run("trims trailing carriage return from Windows paste", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("good-name\r\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptRequired(out, sc, "Name", validateName)
		require.NoError(t, err)
		assert.Equal(t, "good-name", got)
	})
}

func TestPromptOptional(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns default", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptOptional(out, sc, "DNS", "10.64.0.1", validateDNS)
		require.NoError(t, err)
		assert.Equal(t, "10.64.0.1", got)
	})

	t.Run("valid input is returned", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("1.1.1.1\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptOptional(out, sc, "DNS", "10.64.0.1", validateDNS)
		require.NoError(t, err)
		assert.Equal(t, "1.1.1.1", got)
	})

	t.Run("invalid input re-prompts, then empty returns default", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("not-an-ip\n\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptOptional(out, sc, "DNS", "10.64.0.1", validateDNS)
		require.NoError(t, err)
		assert.Equal(t, "10.64.0.1", got)
		assert.Contains(t, out.String(), "error:")
	})

	t.Run("EOF returns default value", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptOptional(out, sc, "DNS", "10.64.0.1", validateDNS)
		require.NoError(t, err)
		assert.Equal(t, "10.64.0.1", got)
	})
}

func TestResolveOutputPath(t *testing.T) {
	t.Parallel()

	t.Run("new name in existing dir succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := runConfig{dir: dir}
		got, err := resolveOutputPath(cfg, "new-tunnel")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "new-tunnel.conf"), got)
	})

	t.Run("existing file without force returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "exists.conf"), []byte("x"), 0o600))
		cfg := runConfig{dir: dir, force: false}
		_, err := resolveOutputPath(cfg, "exists")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("existing file with force returns path", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "exists.conf"), []byte("x"), 0o600))
		cfg := runConfig{dir: dir, force: true}
		got, err := resolveOutputPath(cfg, "exists")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "exists.conf"), got)
	})

	t.Run("missing dir is created", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "new-dir")
		cfg := runConfig{dir: dir}
		_, err := resolveOutputPath(cfg, "t")
		require.NoError(t, err)
		fi, statErr := os.Stat(dir)
		require.NoError(t, statErr)
		assert.True(t, fi.IsDir())
	})
}

func TestBuildConfBody(t *testing.T) {
	t.Parallel()

	t.Run("produced body round-trips through wgconf.Parse", func(t *testing.T) {
		t.Parallel()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		privKey := k.String()
		pubKey := genPubKey(t)

		body := buildConfBody(privKey, "10.66.1.2/32", pubKey, "185.1.2.3:51820", "10.64.0.1", "0.0.0.0/0, ::/0")

		dir := t.TempDir()
		path := filepath.Join(dir, "round-trip.conf")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

		parsed, err := wgconf.Parse(path, discardLog)
		require.NoError(t, err)
		assert.Equal(t, privKey, parsed.Interface.PrivateKey)
		assert.Equal(t, netip.MustParsePrefix("10.66.1.2/32"), parsed.Interface.Addresses[0])
		assert.Equal(t, pubKey, parsed.Peer.PublicKey)
		assert.Equal(t, "185.1.2.3:51820", parsed.Peer.Endpoint)
	})

	t.Run("body contains required section headers", func(t *testing.T) {
		t.Parallel()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		body := buildConfBody(k.String(), "10.66.1.2/32", genPubKey(t), "185.1.2.3:51820", "10.64.0.1", "0.0.0.0/0")
		assert.Contains(t, body, "[Interface]")
		assert.Contains(t, body, "[Peer]")
	})
}

func TestWriteAtomicConf(t *testing.T) {
	t.Parallel()

	t.Run("file is written with mode 0600", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "out.conf")
		require.NoError(t, writeAtomicConf(dir, path, "body"))
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	})

	t.Run("file contents match what was written", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "out.conf")
		require.NoError(t, writeAtomicConf(dir, path, "hello-world"))
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "hello-world", string(got))
	})

	t.Run("no temp file left after success", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "out.conf")
		require.NoError(t, writeAtomicConf(dir, path, "body"))
		matches, err := filepath.Glob(filepath.Join(dir, "*.tmp"))
		require.NoError(t, err)
		assert.Empty(t, matches, "no temp files should remain after successful write")
	})
}

func TestNormalizeAddress(t *testing.T) {
	t.Parallel()

	t.Run("IPv4 with CIDR is returned unchanged", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeAddress("10.66.1.2/32")
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
	})

	t.Run("IPv6 with CIDR is returned unchanged", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeAddress("fc00::5/128")
		require.NoError(t, err)
		assert.Equal(t, "fc00::5/128", got)
	})

	t.Run("bare IPv4 host gets /32 appended", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeAddress("10.66.1.2")
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
	})

	t.Run("bare IPv6 host gets /128 appended", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeAddress("fc00::5")
		require.NoError(t, err)
		assert.Equal(t, "fc00::5/128", got)
	})

	t.Run("garbage returns error", func(t *testing.T) {
		t.Parallel()
		_, err := normalizeAddress("not-an-ip")
		require.Error(t, err)
	})
}

func TestPromptAddress(t *testing.T) {
	t.Parallel()

	t.Run("valid CIDR is accepted and returned", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("10.66.1.2/32\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptAddress(out, sc)
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
	})

	t.Run("bare IPv4 host is normalized to /32", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("10.66.1.2\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptAddress(out, sc)
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
	})

	t.Run("bare IPv6 host is normalized to /128", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("fc00::5\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptAddress(out, sc)
		require.NoError(t, err)
		assert.Equal(t, "fc00::5/128", got)
	})

	t.Run("network CIDR is rejected, re-prompts", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("10.0.0.0/8\n10.66.1.2/32\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptAddress(out, sc)
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
		assert.Contains(t, out.String(), "error:")
	})

	t.Run("empty input re-prompts", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("\n10.66.1.2/32\n")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		got, err := promptAddress(out, sc)
		require.NoError(t, err)
		assert.Equal(t, "10.66.1.2/32", got)
	})

	t.Run("EOF returns error", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("")
		out := &bytes.Buffer{}
		sc := newScanner(in)
		_, err := promptAddress(out, sc)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EOF")
	})
}

// newScanner wraps the provided reader in a bufio.Scanner, mirroring what run()
// does. Extracted here so tests don't need to import bufio.
func newScanner(r *bytes.Buffer) *bufio.Scanner {
	return bufio.NewScanner(r)
}

// zipEntry is a helper type for constructing in-memory test zips.
type zipEntry struct {
	name string
	body string
}

// buildTestZip writes a zip containing the given entries to a temp file and
// returns its path.
func buildTestZip(t *testing.T, entries []zipEntry) string {
	t.Helper()
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")
	f, err := os.Create(zipPath)
	require.NoError(t, err)

	w := zip.NewWriter(f)
	for _, e := range entries {
		fw, wErr := w.Create(e.name)
		require.NoError(t, wErr)
		_, wErr = fw.Write([]byte(e.body))
		require.NoError(t, wErr)
	}
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())
	return zipPath
}

// mullvadConf assembles a minimal Mullvad-style wg-quick conf body (no PrivateKey)
// with the given address, peer public key, and endpoint. PresharedKey is
// included to verify the injector never touches it.
func mullvadConf(address, peerPubKey, endpoint string) string {
	return "[Interface]\n" +
		"Address = " + address + "\n" +
		"DNS = 10.64.0.1\n" +
		"\n" +
		"[Peer]\n" +
		"PublicKey = " + peerPubKey + "\n" +
		"AllowedIPs = 0.0.0.0/0, ::/0\n" +
		"Endpoint = " + endpoint + "\n" +
		"PresharedKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM=\n"
}

// mullvadConfWithKey is like mullvadConf but includes an existing PrivateKey line
// in [Interface] so the injector replaces rather than inserts.
func mullvadConfWithKey(address, privKey, peerPubKey, endpoint string) string {
	return "[Interface]\n" +
		"PrivateKey = " + privKey + "\n" +
		"Address = " + address + "\n" +
		"DNS = 10.64.0.1\n" +
		"\n" +
		"[Peer]\n" +
		"PublicKey = " + peerPubKey + "\n" +
		"AllowedIPs = 0.0.0.0/0, ::/0\n" +
		"Endpoint = " + endpoint + "\n" +
		"PresharedKey = AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM=\n"
}

func TestInjectPrivateKey(t *testing.T) {
	t.Parallel()

	peerKey := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 44-char dummy
	newKey := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA="  // different dummy

	t.Run("existing PrivateKey line is replaced", func(t *testing.T) {
		t.Parallel()
		oldKey := "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDM=" // distinct from peerKey
		input := mullvadConfWithKey("10.1.2.3/32", oldKey, peerKey, "1.2.3.4:51820")
		got, err := injectPrivateKey(input, newKey)
		require.NoError(t, err)
		// the entire body must match byte-for-byte with only the PrivateKey
		// value swapped — no dropped lines, no duplicated key, no reordering.
		want := mullvadConfWithKey("10.1.2.3/32", newKey, peerKey, "1.2.3.4:51820")
		assert.Equal(t, want, got)
	})

	t.Run("case-insensitive key match: lowercase privatekey", func(t *testing.T) {
		t.Parallel()
		oldKey := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCM=" // distinct from peerKey
		body := "[Interface]\nprivatekey = " + oldKey + "\nAddress = 10.1.2.3/32\n\n[Peer]\nPublicKey = " + peerKey + "\nEndpoint = 1.2.3.4:51820\n"
		got, err := injectPrivateKey(body, newKey)
		require.NoError(t, err)
		assert.Contains(t, got, "PrivateKey = "+newKey)
		// old private key value must no longer appear anywhere.
		assert.NotContains(t, got, oldKey)
	})

	t.Run("case-insensitive key match: PRIVATEKEY uppercase", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nPRIVATEKEY = " + peerKey + "\nAddress = 10.1.2.3/32\n\n[Peer]\nPublicKey = " + peerKey + "\nEndpoint = 1.2.3.4:51820\n"
		got, err := injectPrivateKey(body, newKey)
		require.NoError(t, err)
		assert.Contains(t, got, "PrivateKey = "+newKey)
	})

	t.Run("no PrivateKey in Interface: inserted after header", func(t *testing.T) {
		t.Parallel()
		body := mullvadConf("10.1.2.3/32", peerKey, "1.2.3.4:51820")
		got, err := injectPrivateKey(body, newKey)
		require.NoError(t, err)

		// the key must be inserted immediately after the [Interface] header.
		gotLines := strings.Split(got, "\n")
		require.Greater(t, len(gotLines), 1)
		assert.Equal(t, "[Interface]", gotLines[0])
		assert.Equal(t, "PrivateKey = "+newKey, gotLines[1],
			"inserted key must be the first line after [Interface]")
		// exactly one line added, nothing else dropped or reordered.
		assert.Equal(t, len(strings.Split(body, "\n"))+1, len(gotLines))
	})

	t.Run("PresharedKey in Peer is never modified", func(t *testing.T) {
		t.Parallel()
		psk := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM="
		body := "[Interface]\nAddress = 10.1.2.3/32\n\n[Peer]\nPublicKey = " + peerKey + "\nEndpoint = 1.2.3.4:51820\nPresharedKey = " + psk + "\n"
		got, err := injectPrivateKey(body, newKey)
		require.NoError(t, err)
		assert.Contains(t, got, "PresharedKey = "+psk, "PresharedKey must be byte-identical in output")
		assert.Contains(t, got, "PrivateKey = "+newKey)
	})

	t.Run("PrivateKey-looking line in Peer is not modified", func(t *testing.T) {
		t.Parallel()
		// pathological conf: both sections have a PrivateKey line.
		body := "[Interface]\nPrivateKey = " + peerKey + "\nAddress = 10.1.2.3/32\n\n[Peer]\nPrivateKey = " + peerKey + "\nPublicKey = " + peerKey + "\nEndpoint = 1.2.3.4:51820\n"
		got, err := injectPrivateKey(body, newKey)
		require.NoError(t, err)
		// interface one replaced.
		lines := strings.Split(got, "\n")
		interfaceKey := ""
		peerKeyLine := ""
		inIface := false
		for _, l := range lines {
			trimmed := strings.TrimSpace(l)
			if strings.EqualFold(trimmed, "[Interface]") {
				inIface = true
				continue
			}
			if strings.EqualFold(trimmed, "[Peer]") {
				inIface = false
				continue
			}
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(l)), "privatekey") {
				if inIface && interfaceKey == "" {
					interfaceKey = l
				} else if !inIface && peerKeyLine == "" {
					peerKeyLine = l
				}
			}
		}
		assert.Contains(t, interfaceKey, newKey, "Interface PrivateKey must be replaced")
		assert.Contains(t, peerKeyLine, peerKey, "Peer PrivateKey must be unchanged")
	})

	t.Run("missing Interface section returns error", func(t *testing.T) {
		t.Parallel()
		body := "[Peer]\nPublicKey = " + peerKey + "\nEndpoint = 1.2.3.4:51820\n"
		_, err := injectPrivateKey(body, newKey)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[Interface]")
	})
}

func TestReadZipEntries(t *testing.T) {
	t.Parallel()

	peerKey := genPubKey(t)

	t.Run("two conf entries returned with correct names and bodies", func(t *testing.T) {
		t.Parallel()
		body1 := mullvadConf("10.1.1.1/32", peerKey, "1.2.3.4:51820")
		body2 := mullvadConf("10.1.1.1/32", peerKey, "5.6.7.8:51820")
		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: body1},
			{name: "de-001.conf", body: body2},
		})

		entries, err := readZipEntries(zipPath)
		require.NoError(t, err)
		require.Len(t, entries, 2)

		names := map[string]string{entries[0].name: entries[0].body, entries[1].name: entries[1].body}
		assert.Equal(t, body1, names["se-001"])
		assert.Equal(t, body2, names["de-001"])
	})

	t.Run("nested path entry: name derived from basename only", func(t *testing.T) {
		t.Parallel()
		body := mullvadConf("10.1.1.1/32", peerKey, "1.2.3.4:51820")
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "nested.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		fw, err := w.Create("subdir/se-001.conf")
		require.NoError(t, err)
		_, err = fw.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		entries, err := readZipEntries(zipPath)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "se-001", entries[0].name)
	})

	t.Run("non-conf entries and directories are skipped", func(t *testing.T) {
		t.Parallel()
		body := mullvadConf("10.1.1.1/32", peerKey, "1.2.3.4:51820")
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "mixed.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		// directory entry
		_, err = w.Create("docs/")
		require.NoError(t, err)
		// README
		rfw, err := w.Create("README.txt")
		require.NoError(t, err)
		_, err = rfw.Write([]byte("readme"))
		require.NoError(t, err)
		// actual conf
		cfw, err := w.Create("se-001.conf")
		require.NoError(t, err)
		_, err = cfw.Write([]byte(body))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		entries, err := readZipEntries(zipPath)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "se-001", entries[0].name)
	})

	t.Run("zero conf entries returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "empty.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		fw, err := w.Create("README.txt")
		require.NoError(t, err)
		_, err = fw.Write([]byte("nothing here"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		_, err = readZipEntries(zipPath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no .conf entries")
	})

	t.Run("entry name failing validateName is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "bad.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		// a hidden file — the dot prefix fails validateName.
		fw, err := w.Create(".hidden.conf")
		require.NoError(t, err)
		_, err = fw.Write([]byte("x"))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		_, err = readZipEntries(zipPath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid name")
	})

	t.Run("entry exceeding per-entry size cap returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "big.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		fw, err := w.Create("big.conf")
		require.NoError(t, err)
		// write zipMaxEntryBytes+1 bytes.
		_, err = fw.Write(make([]byte, zipMaxEntryBytes+1))
		require.NoError(t, err)
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		_, err = readZipEntries(zipPath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "limit")
	})

	t.Run("more than zipMaxEntries entries returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		zipPath := filepath.Join(dir, "many.zip")
		f, err := os.Create(zipPath)
		require.NoError(t, err)
		w := zip.NewWriter(f)
		for i := 0; i <= zipMaxEntries; i++ {
			name := "srv" + strconv.Itoa(i) + ".conf"
			fw, err := w.Create(name)
			require.NoError(t, err)
			_, err = fw.Write([]byte("[Interface]\n"))
			require.NoError(t, err)
		}
		require.NoError(t, w.Close())
		require.NoError(t, f.Close())

		_, err = readZipEntries(zipPath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), strconv.Itoa(zipMaxEntries))
	})
}

func TestIngestZip(t *testing.T) {
	t.Parallel()

	t.Run("happy path: 3 entries written at 0600 with injected key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		entries := []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
			{name: "de-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "5.6.7.8:51820")},
			{name: "nl-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "9.10.11.12:51820")},
		}
		zipPath := buildTestZip(t, entries)
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		require.Len(t, names, 3)

		privStr := k.String()
		for _, n := range names {
			path := filepath.Join(dir, n+".conf")
			fi, statErr := os.Stat(path)
			require.NoError(t, statErr)
			assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm(), "file mode must be 0600")

			raw, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Contains(t, string(raw), "PrivateKey = "+privStr)
		}
	})

	t.Run("Peer block preserved verbatim including PresharedKey", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)
		psk := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAM="
		body := "[Interface]\n" +
			"Address = 10.66.1.1/32\n" +
			"DNS = 10.64.0.1\n" +
			"\n" +
			"[Peer]\n" +
			"PublicKey = " + peerKey + "\n" +
			"AllowedIPs = 0.0.0.0/0, ::/0\n" +
			"Endpoint = 1.2.3.4:51820\n" +
			"PresharedKey = " + psk + "\n"
		zipPath := buildTestZip(t, []zipEntry{{name: "se-001.conf", body: body}})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		require.Len(t, names, 1)

		raw, readErr := os.ReadFile(filepath.Join(dir, "se-001.conf"))
		require.NoError(t, readErr)
		content := string(raw)
		assert.Contains(t, content, "PresharedKey = "+psk, "PresharedKey must survive verbatim")
		assert.Contains(t, content, "PrivateKey = "+k.String())
	})

	t.Run("existing file without force returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		// pre-create the target file.
		require.NoError(t, os.WriteFile(filepath.Join(dir, "se-001.conf"), []byte("old"), 0o600))

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		_, ingestErr := ingestZip(cfg, k, zipPath)
		require.Error(t, ingestErr)
		assert.Contains(t, ingestErr.Error(), "already exists")

		// original file untouched.
		raw, readErr := os.ReadFile(filepath.Join(dir, "se-001.conf"))
		require.NoError(t, readErr)
		assert.Equal(t, "old", string(raw))
	})

	t.Run("existing file with force is overwritten", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		require.NoError(t, os.WriteFile(filepath.Join(dir, "se-001.conf"), []byte("old"), 0o600))

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: true, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		require.Len(t, names, 1)

		raw, readErr := os.ReadFile(filepath.Join(dir, "se-001.conf"))
		require.NoError(t, readErr)
		assert.Contains(t, string(raw), "PrivateKey = "+k.String())
	})

	t.Run("broken entry (missing Peer) causes round-trip error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)

		// conf without [Peer] will fail wgconf.Parse.
		brokenBody := "[Interface]\nAddress = 10.1.1.1/32\n"
		zipPath := buildTestZip(t, []zipEntry{{name: "broken.conf", body: brokenBody}})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		_, ingestErr := ingestZip(cfg, k, zipPath)
		require.Error(t, ingestErr)
		assert.Contains(t, ingestErr.Error(), "broken")
	})

	t.Run("returned name list matches conf basenames", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "aa.conf", body: mullvadConf("10.1.1.1/32", peerKey, "1.2.3.4:51820")},
			{name: "bb.conf", body: mullvadConf("10.1.1.1/32", peerKey, "5.6.7.8:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.ElementsMatch(t, []string{"aa", "bb"}, names)
	})

	t.Run("private key never appears in output or errors", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.1.1.1/32", peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		_, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.NotContains(t, out.String(), k.String(), "private key must not appear in output")
	})

	t.Run("differing Interface.Addresses triggers warning", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
			{name: "de-001.conf", body: mullvadConf("10.66.2.2/32", peerKey, "5.6.7.8:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		require.Len(t, names, 2)
		assert.Contains(t, out.String(), "WARNING", "differing addresses must produce a warning")
		assert.Contains(t, out.String(), "Interface.Addresses")
	})

	t.Run("identical Interface.Addresses produces no warning", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
			{name: "de-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "5.6.7.8:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		_, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.NotContains(t, out.String(), "WARNING")
	})

	t.Run("prefix is prepended to every written conf name", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "al-tia-wg-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
			{name: "se-sto-wg-002.conf", body: mullvadConf("10.66.1.1/32", peerKey, "5.6.7.8:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, prefix: "mullvad", isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.ElementsMatch(t, []string{"mullvad-al-tia-wg-001", "mullvad-se-sto-wg-002"}, names)

		for _, n := range names {
			_, statErr := os.Stat(filepath.Join(dir, n+".conf"))
			require.NoError(t, statErr, "prefixed file must exist on disk")
		}
	})

	t.Run("trailing separator on prefix is not doubled", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "al-tia-wg-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, prefix: "mullvad-", isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.Equal(t, []string{"mullvad-al-tia-wg-001"}, names)
	})

	t.Run("empty prefix leaves names unchanged", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "al-tia-wg-001.conf", body: mullvadConf("10.66.1.1/32", peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, k, zipPath)
		require.NoError(t, ingestErr)
		assert.Equal(t, []string{"al-tia-wg-001"}, names)
	})

	t.Run("embedded private key is preserved, not clobbered by generated key", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		generated, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		embedded, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConfWithKey("10.66.1.1/32", embedded.String(), peerKey, "1.2.3.4:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		names, ingestErr := ingestZip(cfg, generated, zipPath)
		require.NoError(t, ingestErr)
		require.Len(t, names, 1)

		raw, readErr := os.ReadFile(filepath.Join(dir, "se-001.conf"))
		require.NoError(t, readErr)
		content := string(raw)
		assert.Contains(t, content, "PrivateKey = "+embedded.String(), "embedded key must survive")
		assert.NotContains(t, content, generated.String(), "generated key must not be injected over an embedded one")
		assert.Contains(t, out.String(), embedded.PublicKey().String(), "summary must show the embedded key's public key")
	})

	t.Run("zip mixing two embedded keys is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		k, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		keyA, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		keyB, err := wgtypes.GeneratePrivateKey()
		require.NoError(t, err)
		peerKey := genPubKey(t)

		zipPath := buildTestZip(t, []zipEntry{
			{name: "se-001.conf", body: mullvadConfWithKey("10.66.1.1/32", keyA.String(), peerKey, "1.2.3.4:51820")},
			{name: "de-001.conf", body: mullvadConfWithKey("10.66.2.2/32", keyB.String(), peerKey, "5.6.7.8:51820")},
		})
		out := &bytes.Buffer{}
		cfg := runConfig{dir: dir, force: false, out: out, in: &bytes.Buffer{}, isTTY: func() bool { return true }}

		_, ingestErr := ingestZip(cfg, k, zipPath)
		require.Error(t, ingestErr)
		assert.Contains(t, ingestErr.Error(), "more than one public key")

		matches, globErr := filepath.Glob(filepath.Join(dir, "*.conf"))
		require.NoError(t, globErr)
		assert.Empty(t, matches, "nothing must be written when the zip mixes devices")
	})
}

func TestApplyPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prefix string
		in     string
		want   string
	}{
		{"empty prefix returns name unchanged", "", "al-tia-wg-001", "al-tia-wg-001"},
		{"plain prefix joined with hyphen", "mullvad", "al-tia-wg-001", "mullvad-al-tia-wg-001"},
		{"trailing hyphen not doubled", "mullvad-", "al-tia-wg-001", "mullvad-al-tia-wg-001"},
		{"trailing underscore trimmed", "mullvad_", "al-tia-wg-001", "mullvad-al-tia-wg-001"},
		{"trailing dot trimmed", "mullvad.", "al-tia-wg-001", "mullvad-al-tia-wg-001"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, applyPrefix(tc.prefix, tc.in))
		})
	}
}

func TestExtractInterfacePrivateKey(t *testing.T) {
	t.Parallel()

	t.Run("returns the [Interface] PrivateKey value", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nPrivateKey = abc123=\nAddress = 10.1.1.1/32\n[Peer]\nPublicKey = def456=\n"
		assert.Equal(t, "abc123=", extractInterfacePrivateKey(body))
	})

	t.Run("returns empty when no PrivateKey line exists", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nAddress = 10.1.1.1/32\n[Peer]\nPublicKey = def456=\n"
		assert.Equal(t, "", extractInterfacePrivateKey(body))
	})

	t.Run("ignores PresharedKey in [Peer]", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nAddress = 10.1.1.1/32\n[Peer]\nPublicKey = def456=\nPresharedKey = psk123=\n"
		assert.Equal(t, "", extractInterfacePrivateKey(body))
	})

	t.Run("does not pick up a PrivateKey that only appears in [Peer]", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nAddress = 10.1.1.1/32\n[Peer]\nPrivateKey = stray=\nPublicKey = def456=\n"
		assert.Equal(t, "", extractInterfacePrivateKey(body))
	})

	t.Run("case-insensitive key name match", func(t *testing.T) {
		t.Parallel()
		body := "[Interface]\nprivatekey = abc123=\n"
		assert.Equal(t, "abc123=", extractInterfacePrivateKey(body))
	})
}

func TestValidateZipPath(t *testing.T) {
	t.Parallel()

	t.Run("existing regular zip file is accepted", func(t *testing.T) {
		t.Parallel()
		zipPath := buildTestZip(t, nil) // empty but valid zip
		// even an empty zip is a regular file with .zip extension.
		assert.NoError(t, validateZipPath(zipPath))
	})

	t.Run("empty path is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateZipPath(""))
	})

	t.Run("non-.zip extension is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		p := filepath.Join(dir, "archive.tar.gz")
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		assert.Error(t, validateZipPath(p))
	})

	t.Run("non-existent file is rejected", func(t *testing.T) {
		t.Parallel()
		assert.Error(t, validateZipPath("/nonexistent/path/file.zip"))
	})

	t.Run("directory with .zip extension is rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		zipDir := filepath.Join(dir, "archive.zip")
		require.NoError(t, os.Mkdir(zipDir, 0o700))
		assert.Error(t, validateZipPath(zipDir))
	})
}

func TestPromptMode(t *testing.T) {
	t.Parallel()

	t.Run("choice 1 returns manual mode", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("1\n")
		out := &bytes.Buffer{}
		got, err := promptMode(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, modeManual, got)
	})

	t.Run("empty input defaults to manual mode", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("\n")
		out := &bytes.Buffer{}
		got, err := promptMode(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, modeManual, got)
	})

	t.Run("choice 2 returns zip mode", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("2\n")
		out := &bytes.Buffer{}
		got, err := promptMode(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, modeZip, got)
	})

	t.Run("invalid input re-prompts then accepts valid", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("9\n2\n")
		out := &bytes.Buffer{}
		got, err := promptMode(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, modeZip, got)
		assert.Contains(t, out.String(), "error: enter 1 or 2")
	})

	t.Run("EOF returns error", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("")
		out := &bytes.Buffer{}
		_, err := promptMode(out, newScanner(in))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "EOF")
	})
}

func TestPromptPrefix(t *testing.T) {
	t.Parallel()

	t.Run("non-empty value is returned", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("mullvad\n")
		out := &bytes.Buffer{}
		got, err := promptPrefix(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, "mullvad", got)
	})

	t.Run("blank input means no prefix", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("\n")
		out := &bytes.Buffer{}
		got, err := promptPrefix(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, "", got)
	})

	t.Run("EOF means no prefix, not an error", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("")
		out := &bytes.Buffer{}
		got, err := promptPrefix(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, "", got)
	})

	t.Run("invalid value re-prompts then accepts", func(t *testing.T) {
		t.Parallel()
		in := bytes.NewBufferString("bad/name\nmullvad\n")
		out := &bytes.Buffer{}
		got, err := promptPrefix(out, newScanner(in))
		require.NoError(t, err)
		assert.Equal(t, "mullvad", got)
		assert.Contains(t, out.String(), "error:")
	})
}
