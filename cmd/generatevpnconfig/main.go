// Command generatevpnconfig is an interactive operator CLI that generates a
// WireGuard keypair locally, prints the public key with Mullvad onboarding
// instructions, and assembles .conf files via one of two modes:
//
//   - Manual: prompts for each wg-quick field individually (Address, Peer
//     PublicKey, Endpoint, DNS, AllowedIPs).
//   - Zip ingest: reads a Mullvad-downloaded zip of wg-quick .conf files and
//     writes one .conf per server entry into configs/tunnels/. Entries that
//     already carry a valid [Interface] private key are preserved as-is; only
//     keyless entries get the locally-generated private key injected. This
//     supports both a key-embedded Mullvad download and the keyless flow where
//     a single locally-held keypair fans out across many servers without
//     burning Mullvad's device limit. The tool refuses to write if the zip
//     entries derive to more than one public key (a mixed-device zip), and
//     prints the common public key so it can be cross-checked against Mullvad.
//
// The private key never appears in any log or terminal output — only in the
// written .conf files. The tool refuses to run without a TTY so it cannot
// hang in CI or systemd.
//
// Usage:
//
//	generatevpnconfig [-dir configs/tunnels] [-force]
//
// In zip-ingest mode the tool prompts interactively for an optional name
// prefix; a blank answer keeps the Mullvad names verbatim, while a value such
// as "mullvad" is prepended with a single '-' separator, so al-tia-wg-001.conf
// is written as mullvad-al-tia-wg-001.conf.
package main

import (
	"archive/zip"
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"vpntunnel/internal/infrastructure/wireguard/wgconf"
)

func main() {
	cfg := runConfig{
		in:  os.Stdin,
		out: os.Stdout,
	}
	flag.StringVar(&cfg.dir, "dir", "configs/tunnels", "directory where .conf files are written")
	flag.BoolVar(&cfg.force, "force", false, "overwrite an existing .conf file")
	flag.Parse()

	cfg.isTTY = func() bool {
		f, ok := cfg.in.(*os.File)
		if !ok {
			return false
		}
		fi, err := f.Stat()
		if err != nil {
			return false
		}
		return fi.Mode()&os.ModeCharDevice != 0
	}

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const (
	modeManual = "1"
	modeZip    = "2"

	// zipMaxEntries is the maximum number of .conf entries accepted from a zip.
	// sized to admit a full Mullvad all-servers download (~700 today) with
	// headroom, while still bounding a malicious zip's entry count.
	zipMaxEntries = 2048
	// zipMaxEntryBytes is the maximum decompressed size per .conf entry.
	// Mullvad confs are well under 1 KB; 8 KB is generous headroom.
	zipMaxEntryBytes = 8 * 1024
)

// safeNameRe is the regexp that conf names must match: alphanumeric, dot,
// underscore, and hyphen only. No slashes, no leading dot.
var safeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// runConfig holds injected dependencies for run, making it testable without a
// real terminal.
type runConfig struct {
	in    io.Reader
	out   io.Writer
	dir   string
	force bool
	// prefix is prepended (with a single '-' separator) to each zip-ingested
	// conf name. Empty means names are taken verbatim from the zip entries.
	prefix string
	// isTTY reports whether the current stdin is an interactive terminal.
	// injected at construction so tests bypass the real fd check.
	isTTY func() bool
}

// zipConf is a single .conf entry extracted in memory from a zip archive.
type zipConf struct {
	// name is the sanitized basename of the entry without the .conf extension.
	name string
	// body is the full decompressed content of the .conf file.
	body string
}

// preparedConf is a zip entry resolved to its final on-disk name and body,
// ready to write. body already carries the correct [Interface] private key.
type preparedConf struct {
	name string
	body string
}

// run is the testable entry point. It generates a WireGuard keypair, prints
// onboarding instructions, asks for a mode (manual or zip-ingest), then either
// collects fields interactively or reads a Mullvad zip. It never calls os.Exit.
func run(cfg runConfig) error {
	if !cfg.isTTY() {
		return errors.New("generatevpnconfig: interactive terminal required; " +
			"this tool prompts field-by-field and must be run from a TTY")
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return fmt.Errorf("generate keypair: %w", err)
	}
	pub := key.PublicKey()

	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "=== WireGuard Keypair Generated ===")
	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "Your public key (share this with Mullvad):")
	fmt.Fprintln(cfg.out, pub.String())
	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "Mullvad onboarding steps:")
	fmt.Fprintln(cfg.out, "  1. Open https://mullvad.net/account → WireGuard keys.")
	fmt.Fprintln(cfg.out, "  2. Click 'Add key' and paste the public key above.")
	fmt.Fprintln(cfg.out, "")

	sc := bufio.NewScanner(cfg.in)

	// ask the operator which assembly mode to use.
	mode, err := promptMode(cfg.out, sc)
	if err != nil {
		return fmt.Errorf("mode selection: %w", err)
	}

	if mode == modeZip {
		return runZipMode(cfg, key, pub, sc)
	}
	return runManualMode(cfg, key, pub, sc)
}

// runManualMode is the original field-by-field interactive flow.
func runManualMode(cfg runConfig, key wgtypes.Key, pub wgtypes.Key, sc *bufio.Scanner) error {
	fmt.Fprintln(cfg.out, "  3. Go to https://mullvad.net/en/servers and pick a server.")
	fmt.Fprintln(cfg.out, "  4. Note the assigned Address (e.g. 10.66.x.x/32),")
	fmt.Fprintln(cfg.out, "     the server Peer PublicKey, and the Endpoint (host:port).")
	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "Now enter the values from Mullvad to assemble your .conf:")
	fmt.Fprintln(cfg.out, "")

	name, err := promptRequired(cfg.out, sc, "Config name (e.g. mullvad-se-sto-wg-001)", validateName)
	if err != nil {
		return fmt.Errorf("conf name: %w", err)
	}

	address, err := promptAddress(cfg.out, sc)
	if err != nil {
		return fmt.Errorf("address: %w", err)
	}

	peerPubKey, err := promptRequired(cfg.out, sc, "Peer PublicKey (base64)", validatePeerPublicKey)
	if err != nil {
		return fmt.Errorf("peer public key: %w", err)
	}

	endpoint, err := promptRequired(cfg.out, sc, "Endpoint (host:port or [v6]:port)", validateEndpoint)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}

	dns, err := promptOptional(cfg.out, sc, "DNS [default: 10.64.0.1]", "10.64.0.1", validateDNS)
	if err != nil {
		return fmt.Errorf("dns: %w", err)
	}

	allowedIPs, err := promptOptional(cfg.out, sc, "AllowedIPs [default: 0.0.0.0/0, ::/0]", "0.0.0.0/0, ::/0", validateAllowedIPs)
	if err != nil {
		return fmt.Errorf("allowed ips: %w", err)
	}

	outPath, err := resolveOutputPath(cfg, name)
	if err != nil {
		return err
	}

	confBody := buildConfBody(key.String(), address, peerPubKey, endpoint, dns, allowedIPs)

	if err := writeAtomicConf(cfg.dir, outPath, confBody); err != nil {
		return fmt.Errorf("write conf: %w", err)
	}

	// round-trip through the parser to verify the written file is valid.
	// pass a discard logger so a future parser warning can never chatter to
	// stderr behind the operator's back.
	parsed, err := wgconf.Parse(outPath, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return fmt.Errorf("round-trip parse failed — the written .conf is malformed: %w", err)
	}

	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "=== Config Written ===")
	fmt.Fprintf(cfg.out, "File:          %s\n", outPath)
	fmt.Fprintf(cfg.out, "Public key:    %s\n", pub.String())
	fmt.Fprintf(cfg.out, "Local address: %s\n", parsed.Interface.Addresses[0])
	fmt.Fprintf(cfg.out, "Peer endpoint: %s\n", parsed.Peer.Endpoint)
	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "The .conf is already in configs/tunnels/ — vpntunnel auto-discovers it.")
	fmt.Fprintln(cfg.out, "Restart vpntunnel (or start it fresh) and it will pick up the new config.")
	fmt.Fprintln(cfg.out, "")

	return nil
}

// runZipMode handles the zip-ingest flow: prints the warning block, prompts for
// the zip path, calls ingestZip, and prints the multi-entry summary.
func runZipMode(cfg runConfig, key wgtypes.Key, pub wgtypes.Key, sc *bufio.Scanner) error {
	printZipInstructions(cfg.out, pub.String())

	prefix, err := promptPrefix(cfg.out, sc)
	if err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	cfg.prefix = prefix

	zipPath, err := promptRequired(cfg.out, sc, "Path to downloaded Mullvad zip", validateZipPath)
	if err != nil {
		return fmt.Errorf("zip path: %w", err)
	}

	names, err := ingestZip(cfg, key, zipPath)
	if err != nil {
		return err
	}

	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "=== Configs Written ===")
	for _, n := range names {
		fmt.Fprintf(cfg.out, "  %s\n", filepath.Join(cfg.dir, n+".conf"))
	}
	fmt.Fprintln(cfg.out, "")
	fmt.Fprintln(cfg.out, "All configs are in configs/tunnels/ — vpntunnel auto-discovers them.")
	fmt.Fprintln(cfg.out, "Restart vpntunnel (or start it fresh) and it will pick up every new config.")
	fmt.Fprintln(cfg.out, "")

	return nil
}

// applyPrefix returns name with prefix prepended and a single '-' separator,
// e.g. applyPrefix("mullvad", "al-tia-wg-001") == "mullvad-al-tia-wg-001". An
// empty prefix returns name unchanged. Any trailing '-', '_', or '.' on prefix
// is trimmed first so passing "mullvad" or "mullvad-" yields the same result.
func applyPrefix(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return strings.TrimRight(prefix, "-_.") + "-" + name
}

// validateName returns an error if name is not a safe conf basename.
func validateName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if name == "." || name == ".." {
		return errors.New("name must not be '.' or '..'")
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("name must not contain path separators")
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("name must not start with '.'")
	}
	if !safeNameRe.MatchString(name) {
		return errors.New("name must contain only letters, digits, '.', '-', '_'")
	}
	return nil
}

// validateAddress returns an error if addr is not a valid single-host CIDR
// prefix or a bare host address (bare hosts are accepted by the prompt path
// because promptAddress normalizes them to /32 or /128 before returning).
// This validator operates on an already-normalized CIDR string.
func validateAddress(addr string) error {
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		return fmt.Errorf("invalid CIDR: %w", err)
	}
	if prefix.Bits() != prefix.Addr().BitLen() {
		return fmt.Errorf("address must be a single-host prefix (got /%d, want /%d)",
			prefix.Bits(), prefix.Addr().BitLen())
	}
	return nil
}

// promptAddress prompts the operator for the WireGuard interface address,
// normalizes a bare host IP to a /32 or /128 prefix, validates it as a
// single-host CIDR, and returns the normalized form. It re-prompts on error
// and returns an error on EOF.
func promptAddress(out io.Writer, sc *bufio.Scanner) (string, error) {
	for {
		fmt.Fprintf(out, "Address (e.g. 10.66.x.x/32): ")
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return "", errors.New("unexpected EOF — Address is required")
		}
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			fmt.Fprintf(out, "  error: Address is required\n")
			continue
		}
		normalized, normErr := normalizeAddress(raw)
		if normErr != nil {
			fmt.Fprintf(out, "  error: %s\n", normErr)
			continue
		}
		if err := validateAddress(normalized); err != nil {
			fmt.Fprintf(out, "  error: %s\n", err)
			continue
		}
		return normalized, nil
	}
}

// normalizeAddress appends /32 (IPv4) or /128 (IPv6) if addr has no prefix
// length. Returns addr unchanged if it already contains a '/'.
func normalizeAddress(addr string) (string, error) {
	if strings.Contains(addr, "/") {
		return addr, nil
	}
	parsed, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("invalid address: %w", err)
	}
	if parsed.Is4() {
		return addr + "/32", nil
	}
	return addr + "/128", nil
}

// validatePeerPublicKey returns an error if key is not a valid WireGuard
// public key (base64-encoded 32 bytes).
func validatePeerPublicKey(key string) error {
	if _, err := wgtypes.ParseKey(key); err != nil {
		return fmt.Errorf("invalid WireGuard key: %w", err)
	}
	return nil
}

// validateEndpoint returns an error if ep is not a valid "host:port" or
// "[v6]:port" endpoint with a port in 1..65535.
func validateEndpoint(ep string) error {
	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		return fmt.Errorf("invalid endpoint (want host:port or [v6]:port): %w", err)
	}
	if host == "" {
		return errors.New("endpoint host must not be empty")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("endpoint port %q is not numeric: %w", portStr, err)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("endpoint port %d out of range 1..65535", port)
	}
	return nil
}

// validateDNS returns an error if dns is not a valid IP address.
func validateDNS(dns string) error {
	if _, err := netip.ParseAddr(dns); err != nil {
		return fmt.Errorf("invalid DNS address: %w", err)
	}
	return nil
}

// validateAllowedIPs returns an error if any element in the
// comma-separated list is not a valid CIDR prefix.
func validateAllowedIPs(val string) error {
	parts := strings.Split(val, ",")
	valid := 0
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := netip.ParsePrefix(p); err != nil {
			return fmt.Errorf("invalid CIDR %q: %w", p, err)
		}
		valid++
	}
	if valid == 0 {
		return errors.New("at least one CIDR is required; leave blank to use the default")
	}
	return nil
}

// promptRequired writes a prompt to out and reads from sc until the user
// supplies a line that passes validate. On EOF with no valid input it returns
// an error. Trailing whitespace and \r are stripped before validation.
func promptRequired(out io.Writer, sc *bufio.Scanner, label string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(out, "%s: ", label)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return "", fmt.Errorf("unexpected EOF — %s is required", label)
		}
		val := strings.TrimSpace(sc.Text())
		if val == "" {
			fmt.Fprintf(out, "  error: %s is required\n", label)
			continue
		}
		if err := validate(val); err != nil {
			fmt.Fprintf(out, "  error: %s\n", err)
			continue
		}
		return val, nil
	}
}

// promptOptional writes a prompt to out and reads a single line. Empty input
// returns defaultVal. Non-empty input must pass validate or the user is
// re-prompted.
func promptOptional(out io.Writer, sc *bufio.Scanner, label, defaultVal string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(out, "%s: ", label)
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			// EOF on optional field — use default.
			return defaultVal, nil
		}
		val := strings.TrimSpace(sc.Text())
		if val == "" {
			return defaultVal, nil
		}
		if err := validate(val); err != nil {
			fmt.Fprintf(out, "  error: %s\n", err)
			continue
		}
		return val, nil
	}
}

// resolveOutputPath builds the target .conf path and checks overwrite policy.
// It creates cfg.dir if missing, and returns an error when the file already
// exists and cfg.force is false.
func resolveOutputPath(cfg runConfig, name string) (string, error) {
	if err := os.MkdirAll(cfg.dir, 0o700); err != nil {
		return "", fmt.Errorf("create dir %s: %w", cfg.dir, err)
	}

	outPath := filepath.Join(cfg.dir, name+".conf")

	// defense-in-depth: confirm the join stayed inside cfg.dir.
	if filepath.Dir(outPath) != filepath.Clean(cfg.dir) {
		return "", fmt.Errorf("resolved path %s escapes target dir %s", outPath, cfg.dir)
	}

	if _, err := os.Stat(outPath); err == nil {
		// file exists
		if !cfg.force {
			return "", fmt.Errorf(
				"%s already exists — pick another name or re-run with -force to overwrite",
				outPath,
			)
		}
	}

	return outPath, nil
}

// buildConfBody assembles a wg-quick body string. Field order follows the
// canonical wg-quick layout for operator readability; the parser is
// order-insensitive.
func buildConfBody(privateKey, address, peerPublicKey, endpoint, dns, allowedIPs string) string {
	var sb strings.Builder
	sb.WriteString("[Interface]\n")
	fmt.Fprintf(&sb, "PrivateKey = %s\n", privateKey)
	fmt.Fprintf(&sb, "Address = %s\n", address)
	fmt.Fprintf(&sb, "DNS = %s\n", dns)
	sb.WriteString("\n")
	sb.WriteString("[Peer]\n")
	fmt.Fprintf(&sb, "PublicKey = %s\n", peerPublicKey)
	fmt.Fprintf(&sb, "AllowedIPs = %s\n", allowedIPs)
	fmt.Fprintf(&sb, "Endpoint = %s\n", endpoint)
	return sb.String()
}

// writeAtomicConf creates a temp file in dir, chmods it to 0600, writes body,
// syncs and closes it, then renames it to finalPath. This keeps the write
// atomic: finalPath is never partially written. On any error after temp
// creation the temp is removed (best-effort).
func writeAtomicConf(dir, finalPath, body string) error {
	tmp, err := os.CreateTemp(dir, filepath.Base(finalPath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	// track cleanup separately so we can zero it out after the rename succeeds.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}

	if _, err := io.WriteString(tmp, body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}

	if err := os.Rename(tmpName, finalPath); err != nil {
		return fmt.Errorf("rename temp to final: %w", err)
	}
	cleanup = false
	return nil
}

// promptMode writes the assembly-mode menu to out and reads from sc until the
// user supplies "1", "2", or empty (which defaults to "1"). It re-prompts on
// invalid input and returns an error on EOF.
func promptMode(out io.Writer, sc *bufio.Scanner) (string, error) {
	for {
		fmt.Fprintln(out, "Assemble from:  [1] manual field entry  [2] a downloaded Mullvad zip")
		fmt.Fprintf(out, "Choice [1]: ")
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return "", errors.New("unexpected EOF — mode selection is required")
		}
		val := strings.TrimSpace(sc.Text())
		if val == "" || val == modeManual {
			return modeManual, nil
		}
		if val == modeZip {
			return modeZip, nil
		}
		fmt.Fprintf(out, "  error: enter 1 or 2\n")
	}
}

// promptPrefix asks for an optional conf-name prefix. Empty input (or EOF)
// means no prefix and returns "". A non-empty value must be a safe name
// component; the operator is re-prompted on invalid input. It returns an error
// only on a scanner read failure.
func promptPrefix(out io.Writer, sc *bufio.Scanner) (string, error) {
	for {
		fmt.Fprint(out, "Name prefix (optional, e.g. mullvad; blank for none): ")
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return "", fmt.Errorf("read error: %w", err)
			}
			return "", nil
		}
		val := strings.TrimSpace(sc.Text())
		if val == "" {
			return "", nil
		}
		if err := validateName(val); err != nil {
			fmt.Fprintf(out, "  error: %s\n", err)
			continue
		}
		return val, nil
	}
}

// validateZipPath returns an error if path is not a regular readable file with
// a .zip extension.
func validateZipPath(path string) error {
	if path == "" {
		return errors.New("zip path is required")
	}
	if !strings.EqualFold(filepath.Ext(path), ".zip") {
		return fmt.Errorf("file %q does not have a .zip extension", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot stat %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
}

// printZipInstructions writes the zip-mode warning and Mullvad steps to out.
// pubKey is the already-printed public key so the operator can cross-reference.
func printZipInstructions(out io.Writer, pubKey string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "=== Zip Ingest Mode ===")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The zip MUST have been generated for the public key above:")
	fmt.Fprintf(out, "  %s\n", pubKey)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Steps:")
	fmt.Fprintln(out, "  1. On https://mullvad.net/account → WireGuard keys,")
	fmt.Fprintln(out, "     click 'Add key' and paste the public key above if you")
	fmt.Fprintln(out, "     have not done so yet.")
	fmt.Fprintln(out, "  2. Go to https://mullvad.net/en/servers and generate configs")
	fmt.Fprintln(out, "     for that key → download the zip.")
	fmt.Fprintln(out, "  3. Enter the path to the downloaded zip below.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "WARNING: this tool cannot verify offline that the zip was generated")
	fmt.Fprintln(out, "for the public key above. A mismatched zip produces configs that fail")
	fmt.Fprintln(out, "silently — WireGuard rejects the handshake and no session is ever")
	fmt.Fprintln(out, "established, so no traffic passes. Verify via /v1/admin/health.")
	fmt.Fprintln(out, "")
}

// readZipEntries opens the zip at zipPath and returns the sanitized .conf
// entries read entirely in memory. It enforces a per-entry decompressed-size
// cap of zipMaxEntryBytes and an entry-count cap of zipMaxEntries. Non-.conf
// and directory entries are silently skipped. An error is returned if zero
// .conf entries are found.
func readZipEntries(zipPath string) ([]zipConf, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer func() { _ = r.Close() }()

	var entries []zipConf
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}

		base := filepath.Base(f.Name)
		if !strings.EqualFold(filepath.Ext(base), ".conf") {
			continue
		}

		if len(entries) >= zipMaxEntries {
			return nil, fmt.Errorf("zip contains more than %d .conf entries; refusing to process", zipMaxEntries)
		}

		// strip .conf suffix and validate the resulting name.
		candidateName := strings.TrimSuffix(base, filepath.Ext(base))
		if err := validateName(candidateName); err != nil {
			return nil, fmt.Errorf("zip entry %q has invalid name %q: %w", f.Name, candidateName, err)
		}

		body, readErr := readZipEntry(f)
		if readErr != nil {
			return nil, fmt.Errorf("zip entry %q: %w", f.Name, readErr)
		}

		entries = append(entries, zipConf{name: candidateName, body: body})
	}

	if len(entries) == 0 {
		return nil, errors.New("zip contains no .conf entries")
	}
	return entries, nil
}

// readZipEntry reads the decompressed content of a single zip file entry,
// enforcing the zipMaxEntryBytes cap. It returns an error if the entry exceeds
// the cap rather than silently truncating.
func readZipEntry(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("open entry: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// read up to cap+1 so we can detect over-cap vs exactly-cap.
	limited := io.LimitReader(rc, zipMaxEntryBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read entry: %w", err)
	}
	if len(data) > zipMaxEntryBytes {
		return "", fmt.Errorf("decompressed size exceeds %d byte limit", zipMaxEntryBytes)
	}
	return string(data), nil
}

// ingestZip reads all .conf entries from the zip at zipPath and writes one
// .conf per entry to cfg.dir, returning the list of written conf names (without
// extension). For each entry it preserves an already-present valid [Interface]
// private key (never clobbering a Mullvad-embedded key) and only injects key
// into entries that carry no usable private key.
//
// ingestZip runs in two passes. Pass one resolves and validates every entry —
// names, the resulting private key, and the public key it derives to — without
// touching disk: it fails (writing nothing) if the entries derive to more than
// one public key, or if cfg.expectPubKey is set and any entry does not derive
// to it. Pass two writes and round-trip-parses every file. A write/parse error
// in pass two names the offending file; files written before it remain on disk
// and can be overwritten with -force.
//
// After all files are written, an address-consistency check warns (but does not
// fail) if the confs carry different Interface.Addresses, which can indicate a
// zip mixing keyless configs from different accounts.
func ingestZip(cfg runConfig, key wgtypes.Key, zipPath string) ([]string, error) {
	entries, err := readZipEntries(zipPath)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}

	prepared, resultPub, err := prepareEntries(cfg, key, entries)
	if err != nil {
		return nil, err
	}

	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	var names []string
	var parsedCfgs []*wgconf.ParsedConfig
	for _, p := range prepared {
		outPath, pathErr := resolveOutputPath(cfg, p.name)
		if pathErr != nil {
			return nil, fmt.Errorf("entry %q: %w", p.name, pathErr)
		}

		if writeErr := writeAtomicConf(cfg.dir, outPath, p.body); writeErr != nil {
			return nil, fmt.Errorf("entry %q: write conf: %w", p.name, writeErr)
		}

		parsed, parseErr := wgconf.Parse(outPath, discardLog)
		if parseErr != nil {
			return nil, fmt.Errorf("entry %q: round-trip parse failed — the written .conf is malformed: %w", p.name, parseErr)
		}

		names = append(names, p.name)
		parsedCfgs = append(parsedCfgs, parsed)
	}

	fmt.Fprintln(cfg.out, "")
	fmt.Fprintf(cfg.out, "All %d configs use public key: %s\n", len(names), resultPub)
	fmt.Fprintln(cfg.out, "Confirm this key is registered on your Mullvad account.")

	checkAddressConsistency(cfg.out, parsedCfgs)
	return names, nil
}

// prepareEntries resolves every zip entry to its final name and body without
// writing to disk. It preserves an entry's existing valid private key and
// injects key only when none is present, derives the public key each conf will
// use, and returns an error if the entries do not all derive to a single public
// key or if cfg.expectPubKey is set and any entry does not match it. On success
// it returns the prepared entries and the common public key (base64).
func prepareEntries(cfg runConfig, key wgtypes.Key, entries []zipConf) ([]preparedConf, string, error) {
	var prepared []preparedConf
	var resultPub string

	for _, entry := range entries {
		outName := applyPrefix(cfg.prefix, entry.name)
		if err := validateName(outName); err != nil {
			return nil, "", fmt.Errorf("entry %q: prefixed name %q is invalid: %w", entry.name, outName, err)
		}

		body := entry.body
		privStr := extractInterfacePrivateKey(entry.body)
		if _, parseErr := wgtypes.ParseKey(privStr); parseErr != nil {
			// no usable embedded key — inject the locally generated one.
			injected, injErr := injectPrivateKey(entry.body, key.String())
			if injErr != nil {
				return nil, "", fmt.Errorf("entry %q: inject private key: %w", entry.name, injErr)
			}
			body = injected
			privStr = key.String()
		}

		priv, keyErr := wgtypes.ParseKey(privStr)
		if keyErr != nil {
			return nil, "", fmt.Errorf("entry %q: private key is invalid: %w", entry.name, keyErr)
		}
		pub := priv.PublicKey().String()

		if resultPub == "" {
			resultPub = pub
		} else if pub != resultPub {
			return nil, "", fmt.Errorf("entry %q: confs derive to more than one public key (%s vs %s) — the zip mixes devices; refusing to write", entry.name, pub, resultPub)
		}

		prepared = append(prepared, preparedConf{name: outName, body: body})
	}

	return prepared, resultPub, nil
}

// extractInterfacePrivateKey returns the trimmed PrivateKey value from the
// [Interface] section of confBody, or "" if there is no [Interface] PrivateKey
// line. It matches the key name on the left-hand side of the first '=' (lowered
// and trimmed) so PresharedKey in [Peer] is never picked up.
func extractInterfacePrivateKey(confBody string) string {
	inInterface := false
	for _, raw := range strings.Split(confBody, "\n") {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") {
			inInterface = strings.EqualFold(trimmed, "[Interface]")
			continue
		}
		if !inInterface {
			continue
		}
		eq := strings.IndexByte(raw, '=')
		if eq < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(raw[:eq])) == "privatekey" {
			return strings.TrimSpace(raw[eq+1:])
		}
	}
	return ""
}

// checkAddressConsistency prints a warning to out if the ingested configs carry
// different Interface.Addresses values. Differing addresses likely indicate the
// zip was generated for a different key or Mullvad account.
func checkAddressConsistency(out io.Writer, cfgs []*wgconf.ParsedConfig) {
	if len(cfgs) == 0 {
		return
	}

	// build a canonical sorted-string key per address list.
	addrKey := func(c *wgconf.ParsedConfig) string {
		strs := make([]string, len(c.Interface.Addresses))
		for i, p := range c.Interface.Addresses {
			strs[i] = p.String()
		}
		sort.Strings(strs)
		return strings.Join(strs, ",")
	}

	first := addrKey(cfgs[0])
	for _, c := range cfgs[1:] {
		if addrKey(c) != first {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "WARNING: ingested configs have different Interface.Addresses.")
			fmt.Fprintln(out, "This may mean the zip was generated for a different key or account.")
			fmt.Fprintln(out, "Distinct address sets found:")
			seen := map[string]bool{}
			for _, cfg := range cfgs {
				k := addrKey(cfg)
				if !seen[k] {
					fmt.Fprintf(out, "  %s\n", k)
					seen[k] = true
				}
			}
			return
		}
	}
}

// injectPrivateKey returns confBody with the [Interface] PrivateKey value
// replaced by privateKey. If [Interface] has no PrivateKey line, one is
// inserted directly after the [Interface] header. PresharedKey (in [Peer]) is
// never touched. The rest of the body is preserved byte-for-byte (assumes LF
// line endings, which Mullvad confs use). It returns an error if confBody has
// no [Interface] section.
func injectPrivateKey(confBody, privateKey string) (string, error) {
	lines := strings.Split(confBody, "\n")
	inInterface := false
	seenInterface := false
	injected := false
	ifaceHeaderIdx := -1

	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") {
			inInterface = strings.EqualFold(trimmed, "[Interface]")
			if inInterface {
				seenInterface = true
				ifaceHeaderIdx = i
			}
			continue
		}
		if inInterface && !injected {
			if eq := strings.IndexByte(raw, '='); eq >= 0 {
				k := strings.ToLower(strings.TrimSpace(raw[:eq]))
				if k == "privatekey" {
					lines[i] = "PrivateKey = " + privateKey
					injected = true
				}
			}
		}
	}

	if !seenInterface {
		return "", errors.New("conf has no [Interface] section")
	}
	if !injected {
		// single allocation avoids the shared-backing-array hazard of nesting
		// two appends over the original slice.
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:ifaceHeaderIdx+1]...)
		newLines = append(newLines, "PrivateKey = "+privateKey)
		newLines = append(newLines, lines[ifaceHeaderIdx+1:]...)
		lines = newLines
	}
	return strings.Join(lines, "\n"), nil
}
