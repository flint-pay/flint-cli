package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUpgradeReleaseAPIURL      = "https://api.github.com/repos/flint-pay/flint-cli/releases?per_page=100"
	defaultUpgradeReleaseDownloadURL = "https://github.com/flint-pay/flint-cli/releases/download"
	maxReleaseResponseSize           = 2 << 20
	maxChecksumResponseSize          = 1 << 20
	maxUpgradeArchiveSize            = 128 << 20
	maxUpgradeBinarySize             = 128 << 20
)

var releaseVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

type releaseVersion struct {
	raw        string
	major      uint64
	minor      uint64
	patch      uint64
	prerelease string
}

type githubRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func parseReleaseVersion(value string) (releaseVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	match := releaseVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return releaseVersion{}, false
	}
	major, majorErr := strconv.ParseUint(match[1], 10, 64)
	minor, minorErr := strconv.ParseUint(match[2], 10, 64)
	patch, patchErr := strconv.ParseUint(match[3], 10, 64)
	if majorErr != nil || minorErr != nil || patchErr != nil {
		return releaseVersion{}, false
	}
	for _, identifier := range strings.Split(match[4], ".") {
		if len(identifier) > 1 && identifier[0] == '0' && numericIdentifier(identifier) {
			return releaseVersion{}, false
		}
	}
	return releaseVersion{raw: value, major: major, minor: minor, patch: patch, prerelease: match[4]}, true
}

func compareReleaseVersions(left, right releaseVersion) int {
	for _, pair := range [][2]uint64{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if left.prerelease == right.prerelease {
		return 0
	}
	if left.prerelease == "" {
		return 1
	}
	if right.prerelease == "" {
		return -1
	}
	leftParts, rightParts := strings.Split(left.prerelease, "."), strings.Split(right.prerelease, ".")
	for i := 0; i < min(len(leftParts), len(rightParts)); i++ {
		leftPart, rightPart := leftParts[i], rightParts[i]
		if leftPart == rightPart {
			continue
		}
		leftNumeric, rightNumeric := numericIdentifier(leftPart), numericIdentifier(rightPart)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftPart) != len(rightPart) {
				return compareInt(len(leftPart), len(rightPart))
			}
			if leftPart < rightPart {
				return -1
			}
			return 1
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			return strings.Compare(leftPart, rightPart)
		}
	}
	return compareInt(len(leftParts), len(rightParts))
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareInt(left, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func (a *App) localUpgrade(cmd *Command, opts Options) int {
	installed, ok := parseReleaseVersion(a.Info.Version)
	if !ok {
		return a.fail(&CLIError{
			ExitCode: ExitSoftware,
			Type:     "upgrade_error",
			Code:     "UNRELEASED_BUILD",
			Message:  "This Flint CLI build does not have a release version. Install a published release before using flint upgrade.",
		}, opts)
	}

	upgradeTimeout := opts.Timeout
	if len(opts.Raw["timeout"]) == 0 {
		upgradeTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(a.commandContext(), upgradeTimeout)
	defer cancel()
	var latest releaseVersion
	var upgradeErr *CLIError
	a.withUpgradeProgress(ctx, opts, "Checking for a newer Flint CLI release", func() {
		latest, upgradeErr = a.latestStableCLIRelease(ctx)
	})
	if upgradeErr != nil {
		return a.fail(upgradeErr, opts)
	}
	if compareReleaseVersions(installed, latest) >= 0 {
		return a.outputLocal(map[string]any{"data": map[string]any{
			"status":            "current",
			"installed_version": installed.raw,
			"latest_version":    latest.raw,
		}}, cmd, opts)
	}

	executable, err := a.executablePath()
	if err != nil {
		return a.fail(upgradeFailure("EXECUTABLE_NOT_FOUND", "Could not locate the running Flint CLI executable.", err), opts)
	}
	method := detectInstallMethod(executable)
	resolved, resolveErr := filepath.EvalSymlinks(executable)
	if resolveErr != nil && method == "standalone" {
		return a.fail(upgradeFailure("EXECUTABLE_NOT_FOUND", "Could not resolve the running Flint CLI executable.", resolveErr), opts)
	}
	if resolveErr == nil {
		executable = resolved
		if method == "standalone" {
			method = detectInstallMethod(executable)
		}
	}
	if method == "npm" || method == "homebrew" {
		if a.runtimeGOOS == "windows" {
			return a.fail(&CLIError{
				ExitCode: ExitUsage,
				Type:     "upgrade_error",
				Code:     "RESTART_REQUIRED",
				Message:  "Close Flint CLI, then run npm install -g @flintpay/cli@" + latest.raw + ". Windows cannot replace the running platform binary.",
			}, opts)
		}
		if upgradeErr := a.upgradeWithPackageManager(ctx, opts, method, latest.raw, executable); upgradeErr != nil {
			return a.fail(upgradeErr, opts)
		}
		return a.outputLocal(upgradeResult(installed.raw, latest.raw, method), cmd, opts)
	}

	if a.runtimeGOOS != "darwin" && a.runtimeGOOS != "linux" {
		message := "Standalone self-upgrade is supported on macOS and Linux."
		if a.runtimeGOOS == "windows" && a.runtimeGOARCH == "amd64" {
			asset := fmt.Sprintf("%s/%s/flint_%s_windows_amd64.zip", strings.TrimRight(a.upgradeReleaseDownloadURL, "/"), url.PathEscape("cli/v"+latest.raw), latest.raw)
			message = "Close Flint CLI, then download " + asset + " and replace flint.exe after verifying it against checksums.txt from the same release."
		}
		return a.fail(&CLIError{
			ExitCode: ExitUsage,
			Type:     "upgrade_error",
			Code:     "UNSUPPORTED_PLATFORM",
			Message:  message,
		}, opts)
	}
	arch := a.runtimeGOARCH
	if arch != "amd64" && arch != "arm64" {
		return a.fail(&CLIError{
			ExitCode: ExitUsage,
			Type:     "upgrade_error",
			Code:     "UNSUPPORTED_PLATFORM",
			Message:  "Flint CLI does not publish an upgrade archive for this processor architecture.",
		}, opts)
	}

	var binary []byte
	a.withUpgradeProgress(ctx, opts, "Downloading Flint CLI "+latest.raw+" and checking its checksum", func() {
		binary, upgradeErr = a.downloadUpgradeBinary(ctx, latest.raw, a.runtimeGOOS, arch)
	})
	if upgradeErr != nil {
		return a.fail(upgradeErr, opts)
	}
	var replaceErr *CLIError
	a.withUpgradeProgress(ctx, opts, "Verifying and installing Flint CLI "+latest.raw, func() {
		replaceErr = a.replaceExecutableAtomically(ctx, executable, binary, latest.raw)
	})
	if replaceErr != nil {
		return a.fail(replaceErr, opts)
	}
	return a.outputLocal(upgradeResult(installed.raw, latest.raw, "standalone"), cmd, opts)
}

func upgradeResult(previous, latest, method string) map[string]any {
	return map[string]any{"data": map[string]any{
		"status":            "upgraded",
		"installed_version": latest,
		"previous_version":  previous,
		"install_method":    method,
	}}
}

func detectInstallMethod(executable string) string {
	path := strings.ReplaceAll(filepath.ToSlash(executable), `\`, "/")
	if strings.Contains(path, "/node_modules/@flintpay/cli-") {
		return "npm"
	}
	parts := strings.Split(path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "Cellar" && parts[i+1] == "flint" {
			return "homebrew"
		}
	}
	return "standalone"
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	written := len(value)
	if b.limit <= 0 {
		return written, nil
	}
	if len(value) >= b.limit {
		b.data = append(b.data[:0], value[len(value)-b.limit:]...)
		return written, nil
	}
	overflow := len(b.data) + len(value) - b.limit
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, value...)
	return written, nil
}

func runUpgradeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	// Homebrew's single-formula JSON exceeds 2 KiB. Keep enough bounded stdout
	// for metadata parsing while retaining a small diagnostic stderr tail.
	stdout, stderr := &tailBuffer{limit: 64 << 10}, &tailBuffer{limit: 2048}
	command := exec.CommandContext(ctx, name, args...)
	configureUpgradeProcess(command)
	command.WaitDelay = time.Second
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err != nil {
		detail := strings.TrimSpace(string(stderr.data))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout.data))
		}
		if detail != "" {
			err = fmt.Errorf("%s: %w", detail, err)
		}
	}
	return append([]byte(nil), stdout.data...), err
}

type packageManagerCommand struct {
	name string
	args []string
}

func (a *App) upgradeWithPackageManager(ctx context.Context, opts Options, method, version, executable string) *CLIError {
	var ownershipErr *CLIError
	a.withUpgradeProgress(ctx, opts, "Checking the "+method+" installation", func() {
		ownershipErr = a.verifyPackageManagerOwnership(ctx, method, executable)
	})
	if ctx.Err() != nil {
		return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	if ownershipErr != nil {
		return ownershipErr
	}
	var commands []packageManagerCommand
	switch method {
	case "npm":
		commands = append(commands, packageManagerCommand{"npm", []string{"install", "-g", "@flintpay/cli@" + version, "--no-audit", "--no-fund"}})
	case "homebrew":
		commands = append(commands,
			packageManagerCommand{"brew", []string{"update"}},
			packageManagerCommand{"brew", []string{"upgrade", "flint-pay/tap/flint"}},
		)
	default:
		return upgradeFailure("UNKNOWN_INSTALL_METHOD", "Could not determine how Flint CLI was installed.", nil)
	}
	for _, command := range commands {
		message := "Installing Flint CLI " + version + " with " + method
		if command.name == "brew" && command.args[0] == "update" {
			message = "Updating Homebrew package information"
		}
		var err error
		a.withUpgradeProgress(ctx, opts, message, func() {
			_, err = a.runCommand(ctx, command.name, command.args...)
		})
		if err == nil {
			continue
		}
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
		}
		return upgradeFailure("PACKAGE_MANAGER_FAILED", "The "+method+" upgrade failed. Run the package manager directly for more detail.", err)
	}
	if method == "homebrew" {
		var output []byte
		var err error
		a.withUpgradeProgress(ctx, opts, "Locating the Homebrew installation", func() {
			output, err = a.runCommand(ctx, "brew", "--prefix", "flint-pay/tap/flint")
		})
		if ctx.Err() != nil {
			return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
		}
		prefix := strings.TrimSpace(string(output))
		if err != nil || !filepath.IsAbs(prefix) || strings.ContainsAny(prefix, "\r\n") {
			return upgradeFailure("UPGRADE_VERIFICATION_FAILED", "Homebrew completed, but Flint CLI could not locate the installed binary.", err)
		}
		executable = filepath.Join(filepath.Clean(prefix), "bin", "flint")
	}
	var output []byte
	var err error
	a.withUpgradeProgress(ctx, opts, "Verifying installed Flint CLI "+version, func() {
		output, err = a.runCommand(ctx, executable, "version", "--field", "data.cli_version", "--color", "never")
	})
	if ctx.Err() != nil {
		return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	if err == nil && strings.TrimSpace(string(output)) == version {
		return nil
	}
	failure := upgradeFailure("UPGRADE_VERIFICATION_FAILED", "The "+method+" command completed, but Flint CLI could not read the installed version. Expected "+version+". Run flint version to check the installation, then retry flint upgrade.", err)
	details := map[string]any{"install_method": method, "expected_version": version, "executable": executable, "retry_command": "flint upgrade"}
	failure.Details = details
	installed, valid := parseReleaseVersion(string(output))
	if err != nil || !valid {
		return failure
	}
	details["installed_version"] = installed.raw
	failure.Message = fmt.Sprintf("The %s command completed, but Flint CLI %s is installed; expected %s. Run flint version to check the installation, then retry flint upgrade.", method, installed.raw, version)
	if method == "homebrew" {
		// Confirm that the formula is behind before attributing a version mismatch
		// to release propagation; a current formula can also have a stale binary.
		var info []byte
		var infoErr error
		a.withUpgradeProgress(ctx, opts, "Checking the version available through Homebrew", func() {
			info, infoErr = a.runCommand(ctx, "brew", "info", "--json=v2", "flint-pay/tap/flint")
		})
		if ctx.Err() != nil {
			return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
		}
		var feed struct {
			Formulae []struct {
				Versions struct {
					Stable string `json:"stable"`
				} `json:"versions"`
			} `json:"formulae"`
		}
		if infoErr == nil && json.Unmarshal(info, &feed) == nil && len(feed.Formulae) == 1 {
			available, availableOK := parseReleaseVersion(feed.Formulae[0].Versions.Stable)
			expected, expectedOK := parseReleaseVersion(version)
			if availableOK {
				details["available_version"] = available.raw
			}
			if availableOK && expectedOK && compareReleaseVersions(available, expected) < 0 && compareReleaseVersions(installed, expected) < 0 {
				failure.Code = "PACKAGE_MANAGER_RELEASE_PENDING"
				failure.Message = fmt.Sprintf("Flint CLI %s is available on GitHub, but Homebrew currently offers %s. Installed version: %s. The Homebrew release may still be publishing. Wait a few minutes, then run flint upgrade again.", version, available.raw, installed.raw)
			}
		}
	}
	return failure
}

func (a *App) verifyPackageManagerOwnership(ctx context.Context, method, executable string) *CLIError {
	var command packageManagerCommand
	switch method {
	case "npm":
		command = packageManagerCommand{"npm", []string{"root", "-g"}}
	case "homebrew":
		command = packageManagerCommand{"brew", []string{"--cellar", "flint-pay/tap/flint"}}
	default:
		return upgradeFailure("UNKNOWN_INSTALL_METHOD", "Could not determine how Flint CLI was installed.", nil)
	}
	output, err := a.runCommand(ctx, command.name, command.args...)
	root := strings.TrimSpace(string(output))
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolvedRoot
	}
	if err != nil || !filepath.IsAbs(root) || strings.ContainsAny(root, "\r\n") || !pathWithin(root, executable) {
		message := "The " + method + " command on PATH does not own the running Flint CLI installation. Use the package manager and prefix that installed this executable: " + executable
		return upgradeFailure("PACKAGE_MANAGER_MISMATCH", message, err)
	}
	return nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (a *App) latestStableCLIRelease(ctx context.Context) (releaseVersion, *CLIError) {
	raw, err := a.getUpgradeURL(ctx, a.upgradeReleaseAPIURL, maxReleaseResponseSize)
	if err != nil {
		return releaseVersion{}, err
	}
	var releases []githubRelease
	if decodeErr := json.Unmarshal(raw, &releases); decodeErr != nil {
		return releaseVersion{}, upgradeFailure("INVALID_RELEASE_FEED", "GitHub returned an invalid Flint CLI release list.", decodeErr)
	}
	var latest releaseVersion
	found := false
	for _, release := range releases {
		if release.Draft || release.Prerelease || !strings.HasPrefix(release.TagName, "cli/v") {
			continue
		}
		version, valid := parseReleaseVersion(strings.TrimPrefix(release.TagName, "cli/"))
		if !valid || version.prerelease != "" {
			continue
		}
		if !found || compareReleaseVersions(version, latest) > 0 {
			latest = version
			found = true
		}
	}
	if !found {
		return releaseVersion{}, upgradeFailure("RELEASE_NOT_FOUND", "Could not find a stable Flint CLI release.", nil)
	}
	return latest, nil
}

func (a *App) downloadUpgradeBinary(ctx context.Context, version, goos, goarch string) ([]byte, *CLIError) {
	archiveName := fmt.Sprintf("flint_%s_%s_%s.tar.gz", version, goos, goarch)
	tag := url.PathEscape("cli/v" + version)
	base := strings.TrimRight(a.upgradeReleaseDownloadURL, "/") + "/" + tag
	checksums, err := a.getUpgradeURL(ctx, base+"/checksums.txt", maxChecksumResponseSize)
	if err != nil {
		return nil, err
	}
	expected, ok := checksumForArchive(checksums, archiveName)
	if !ok {
		return nil, upgradeFailure("CHECKSUM_NOT_FOUND", "The release checksums do not include "+archiveName+".", nil)
	}
	archive, err := a.getUpgradeURL(ctx, base+"/"+archiveName, maxUpgradeArchiveSize)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], expected) {
		return nil, upgradeFailure("CHECKSUM_MISMATCH", "The downloaded Flint CLI archive failed checksum verification.", nil)
	}
	binary, extractErr := extractFlintBinary(archive)
	if extractErr != nil {
		return nil, upgradeFailure("INVALID_RELEASE_ARCHIVE", "The Flint CLI release archive is invalid.", extractErr)
	}
	return binary, nil
}

func checksumForArchive(raw []byte, archiveName string) ([]byte, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != archiveName || len(fields[0]) != sha256.Size*2 {
			continue
		}
		checksum, err := hex.DecodeString(fields[0])
		if err == nil {
			return checksum, true
		}
	}
	return nil, false
}

func extractFlintBinary(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(filepath.ToSlash(header.Name)) != "flint" {
			continue
		}
		if header.Size < 1 || header.Size > maxUpgradeBinarySize {
			return nil, errors.New("flint binary has an invalid size")
		}
		binary, readErr := io.ReadAll(io.LimitReader(tarReader, maxUpgradeBinarySize+1))
		if readErr != nil {
			return nil, readErr
		}
		if int64(len(binary)) != header.Size {
			return nil, errors.New("flint binary is truncated")
		}
		return binary, nil
	}
	return nil, errors.New("archive does not contain flint")
}

func (a *App) replaceExecutableAtomically(ctx context.Context, executable string, binary []byte, expectedVersion string) *CLIError {
	writeFailure := func(err error) *CLIError {
		message := "Could not replace the Flint CLI executable at " + executable + ". Check that its directory is writable."
		return upgradeFailure("UPGRADE_WRITE_FAILED", message, err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		return writeFailure(err)
	}
	if !info.Mode().IsRegular() {
		return writeFailure(errors.New("the executable path is not a regular file"))
	}
	directory := filepath.Dir(executable)
	temporary, err := os.CreateTemp(directory, ".flint-upgrade-*")
	if err != nil {
		return writeFailure(err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	mode := info.Mode().Perm() | 0o111
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return writeFailure(err)
	}
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return writeFailure(err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return writeFailure(err)
	}
	if err := temporary.Close(); err != nil {
		return writeFailure(err)
	}
	output, verifyErr := a.runCommand(ctx, temporaryPath, "version", "--field", "data.cli_version", "--color", "never")
	if err := ctx.Err(); err != nil {
		return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out; the existing installation was left unchanged.", err)
	}
	if verifyErr != nil || strings.TrimSpace(string(output)) != expectedVersion {
		return upgradeFailure("UPGRADE_VERIFICATION_FAILED", "The downloaded Flint CLI binary did not report the expected release version; the existing installation was left unchanged.", verifyErr)
	}
	if err := os.Rename(temporaryPath, executable); err != nil {
		return writeFailure(err)
	}
	return nil
}

func (a *App) getUpgradeURL(ctx context.Context, target string, limit int64) ([]byte, *CLIError) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, upgradeFailure("INVALID_RELEASE_URL", "The Flint CLI release URL is invalid.", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
	client := a.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, requestErr := client.Do(request)
	if requestErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
		}
		return nil, networkError("RELEASE_DOWNLOAD_FAILED", "Could not download Flint CLI release information.", requestErr)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, networkError("RELEASE_DOWNLOAD_FAILED", fmt.Sprintf("GitHub returned HTTP %d while downloading Flint CLI release information.", response.StatusCode), nil)
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if readErr != nil {
		return nil, networkError("RELEASE_DOWNLOAD_FAILED", "Could not read Flint CLI release information.", readErr)
	}
	if int64(len(raw)) > limit {
		return nil, upgradeFailure("RELEASE_DOWNLOAD_TOO_LARGE", "The Flint CLI release download exceeded its size limit.", nil)
	}
	return raw, nil
}

func upgradeFailure(code, message string, cause error) *CLIError {
	return &CLIError{ExitCode: ExitSoftware, Type: "upgrade_error", Code: code, Message: message, Cause: cause}
}
