package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// cliAuthSalt matches CLI_AUTH_SALT / CLI_TOKEN_SALT in machineId.js and
// dashboardGuard.js. It must stay byte-identical or Go-derived tokens diverge.
const cliAuthSalt = "9r-cli-auth"

// CLIToken reproduces getConsistentMachineId("9r-cli-auth") from machineId.js:
//
//	sha256(rawMachineId + "9r-cli-auth" + cliSecret).hex[:16]
//
// It reads the SAME files Node persists (DATA_DIR/machine-id and
// DATA_DIR/auth/cli-secret) so a token minted by Node is reproducible here.
//
// Unlike Node, Go NEVER creates these files: if either is missing we return ""
// (empty, non-matchable) so the caller fails closed. Node owns file creation on
// first use; before that no valid CLI token can exist to accept anyway.
func CLIToken(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	raw := readTrimmed(filepath.Join(dataDir, "machine-id"))
	secret := readTrimmed(filepath.Join(dataDir, "auth", "cli-secret"))
	if raw == "" || secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw + cliAuthSalt + secret))
	return hex.EncodeToString(sum[:])[:16]
}

// ValidCLIToken reports whether candidate matches the derived CLI token in
// constant time. An empty derived token (missing files) never matches.
func ValidCLIToken(candidate, dataDir string) bool {
	if candidate == "" {
		return false
	}
	want := CLIToken(dataDir)
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(candidate)) == 1
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
