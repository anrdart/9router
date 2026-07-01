package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mintHS256 builds a jose-compatible HS256 token so we test the verifier against
// the exact wire format createDashboardAuthToken produces.
func mintHS256(t *testing.T, secret []byte, payload map[string]any) string {
	t.Helper()
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	body, _ := json.Marshal(payload)
	signing := enc(header) + "." + enc(body)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signing))
	return signing + "." + enc(m.Sum(nil))
}

func TestVerifyJWT(t *testing.T) {
	secret := []byte("test-secret-hex")
	now := time.Unix(1_700_000_000, 0)

	t.Run("valid", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{
			"authenticated": true,
			"iat":           now.Unix() - 10,
			"exp":           now.Unix() + 3600,
		})
		c, err := VerifyJWT(tok, secret, now)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !c.Authenticated || c.ForcePasswordChange {
			t.Errorf("claims = %+v", c)
		}
	})

	t.Run("force_password_change claim", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{
			"authenticated":         true,
			"force_password_change": true,
			"exp":                   now.Unix() + 600,
		})
		c, err := VerifyJWT(tok, secret, now)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !c.ForcePasswordChange {
			t.Error("force_password_change not decoded")
		}
	})

	t.Run("expired", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{"exp": now.Unix() - 1})
		if _, err := VerifyJWT(tok, secret, now); err != ErrExpired {
			t.Errorf("err = %v, want ErrExpired", err)
		}
	})

	t.Run("missing exp", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{"authenticated": true})
		if _, err := VerifyJWT(tok, secret, now); err != ErrExpired {
			t.Errorf("err = %v, want ErrExpired", err)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{"exp": now.Unix() + 60})
		if _, err := VerifyJWT(tok, []byte("other"), now); err != ErrBadSignature {
			t.Errorf("err = %v, want ErrBadSignature", err)
		}
	})

	t.Run("tampered payload", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{"exp": now.Unix() + 60, "authenticated": false})
		// Flip the payload segment to an "authenticated:true" body without re-signing.
		enc := base64.RawURLEncoding.EncodeToString
		forged, _ := json.Marshal(map[string]any{"exp": now.Unix() + 60, "authenticated": true})
		parts := splitDots(tok)
		bad := parts[0] + "." + enc(forged) + "." + parts[2]
		if _, err := VerifyJWT(bad, secret, now); err != ErrBadSignature {
			t.Errorf("err = %v, want ErrBadSignature", err)
		}
	})

	t.Run("wrong alg rejected", func(t *testing.T) {
		// Craft an HS512 token; verifier must refuse based on the alg header.
		enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
		header, _ := json.Marshal(map[string]string{"alg": "HS512", "typ": "JWT"})
		body, _ := json.Marshal(map[string]any{"exp": now.Unix() + 60})
		signing := enc(header) + "." + enc(body)
		m := hmac.New(sha512.New, secret)
		m.Write([]byte(signing))
		tok := signing + "." + enc(m.Sum(nil))
		if _, err := VerifyJWT(tok, secret, now); err != ErrBadAlg && !isBadAlg(err) {
			t.Errorf("err = %v, want ErrBadAlg", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		if _, err := VerifyJWT("not-a-jwt", secret, now); err != ErrMalformedToken {
			t.Errorf("err = %v, want ErrMalformedToken", err)
		}
	})

	t.Run("no secret fails closed", func(t *testing.T) {
		tok := mintHS256(t, secret, map[string]any{"exp": now.Unix() + 60})
		if _, err := VerifyJWT(tok, nil, now); err != ErrNoSecret {
			t.Errorf("err = %v, want ErrNoSecret", err)
		}
	})
}

func TestLoadJWTSecret(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "  from-env  ")
		if got := string(LoadJWTSecret("/nonexistent")); got != "from-env" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("reads file when env unset", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "jwt-secret"), []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := string(LoadJWTSecret(dir)); got != "file-secret" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("missing file → nil", func(t *testing.T) {
		t.Setenv("JWT_SECRET", "")
		if LoadJWTSecret(t.TempDir()) != nil {
			t.Error("expected nil secret when file missing")
		}
	})
}

func TestCLIToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte("raw-machine-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := filepath.Join(dir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "cli-secret"), []byte("secret-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Independent recomputation of the Node formula.
	sum := sha256.Sum256([]byte("raw-machine-123" + cliAuthSalt + "secret-abc"))
	want := hex.EncodeToString(sum[:])[:16]

	if got := CLIToken(dir); got != want {
		t.Errorf("CLIToken = %q, want %q", got, want)
	}
	if !ValidCLIToken(want, dir) {
		t.Error("ValidCLIToken should accept the derived token")
	}
	if ValidCLIToken("deadbeefdeadbeef", dir) {
		t.Error("ValidCLIToken should reject a wrong token")
	}
	if ValidCLIToken("", dir) {
		t.Error("empty candidate must be rejected")
	}

	// Missing files → empty token, fails closed.
	empty := t.TempDir()
	if CLIToken(empty) != "" {
		t.Error("CLIToken should be empty when files are missing")
	}
	if ValidCLIToken(want, empty) {
		t.Error("no files → never valid")
	}
}

// helpers -------------------------------------------------------------------

func splitDots(s string) [3]string {
	var out [3]string
	i := 0
	for _, seg := range splitN(s, '.', 3) {
		out[i] = seg
		i++
	}
	return out
}

func splitN(s string, sep byte, n int) []string {
	var res []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == sep && len(res) < n-1 {
			res = append(res, cur)
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	res = append(res, cur)
	return res
}

func isBadAlg(err error) bool {
	return err != nil && err != ErrMalformedToken && err != ErrBadSignature && err != ErrExpired && err != ErrNoSecret
}
