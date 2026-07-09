package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// A missing config is not an error: the tool must work out of the box, and the
// first `config set-context` creates the file.
func TestLoadMissingFileYieldsDefault(t *testing.T) {
	t.Parallel()
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentContext != "default" || cfg.Contexts["default"].Server != DefaultServer {
		t.Fatalf("default config = %+v", cfg)
	}
}

func TestLoadAndResolve(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `
apiVersion: v1
kind: Config
current-context: staging
contexts:
  staging:
    server: http://localhost:8080
  prod:
    server: https://metrics.example.com
    auth:
      api-key: secret
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	name, ctx, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if name != "staging" || ctx.Server != "http://localhost:8080" {
		t.Fatalf("resolved %q -> %+v", name, ctx)
	}

	name, ctx, err = cfg.Resolve("prod")
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if name != "prod" || ctx.Auth.APIKey != "secret" {
		t.Fatalf("resolved %q -> %+v", name, ctx)
	}

	if _, _, err := cfg.Resolve("nope"); err == nil {
		t.Fatal("resolving an unknown context must fail")
	}
	if got := cfg.Names(); len(got) != 2 || got[0] != "prod" || got[1] != "staging" {
		t.Fatalf("Names() = %v, want sorted", got)
	}
}

// A config can carry ${VAR} placeholders instead of secrets, so it can be
// checked in and filled from CI or a vault.
func TestLoadExpandsEnvironment(t *testing.T) {
	t.Setenv("METRICSCTL_TEST_TOKEN", "from-env")
	path := writeConfig(t, `
current-context: prod
contexts:
  prod:
    server: https://metrics.example.com
    auth:
      token: ${METRICSCTL_TEST_TOKEN}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Contexts["prod"].Auth.Token; got != "from-env" {
		t.Fatalf("token = %q, want the expanded value", got)
	}
}

// Only the ${NAME} form is expanded. os.ExpandEnv would also eat the bare $NAME
// form, silently truncating any credential that contains a dollar sign.
func TestExpandEnvLeavesDollarsInSecretsAlone(t *testing.T) {
	t.Setenv("METRICSCTL_TEST_SET", "value")
	tests := map[string]string{
		`S3cr$t!`:                  `S3cr$t!`, // $t is not a placeholder
		`pa$$w0rd`:                 `pa$$w0rd`,
		`${METRICSCTL_TEST_SET}`:   `value`,
		`${METRICSCTL_TEST_NONE}`:  `${METRICSCTL_TEST_NONE}`, // unset: left visible
		`a${METRICSCTL_TEST_SET}b`: `avalueb`,
	}
	for in, want := range tests {
		if got := expandEnv(in); got != want {
			t.Errorf("expandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadPreservesADollarInAToken(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, "contexts:\n  a:\n    server: http://x\n    auth:\n      token: \"S3cr$t!\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Contexts["a"].Auth.Token; got != `S3cr$t!` {
		t.Fatalf("token = %q, want it untouched", got)
	}
}

// Two credentials would both be sent; the unused one leaks to the server.
func TestValidateRejectsTwoCredentials(t *testing.T) {
	t.Parallel()
	yaml := "contexts:\n  a:\n    server: http://x\n    auth:\n      api-key: k\n      token: t\n"
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Fatal("a context with two credentials was accepted")
	}
}

// A token-file supersedes everything else; the API key must not tag along.
func TestCredentialReturnsExactlyOne(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte("t0k"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx := &Context{Server: "http://x", Auth: Auth{APIKey: "leak", TokenFile: path}}
	key, token, err := ctx.Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if key != "" || token != "t0k" {
		t.Fatalf("Credential = (%q, %q), want only the token", key, token)
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		yaml    string
		wantSub string
	}{
		"bad apiVersion": {"apiVersion: v2\ncontexts:\n  a:\n    server: http://x\n", "unsupported apiVersion"},
		"no contexts":    {"apiVersion: v1\ncontexts: {}\n", "no contexts"},
		"no server":      {"contexts:\n  a:\n    server: \"\"\n", "has no server"},
		"dangling current": {
			"current-context: nope\ncontexts:\n  a:\n    server: http://x\n", "is not defined",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// The file holds credentials, so it must never be group- or world-readable.
func TestSaveUses0600(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	cfg := Default()
	cfg.Contexts["default"].Auth.Token = "s3cr3t"

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 600", perm)
	}
	if InsecurePermissions(path) {
		t.Fatal("a 0600 file must not be reported insecure")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if !InsecurePermissions(path) {
		t.Fatal("a 0644 credential file must be reported insecure")
	}
}

// os.WriteFile only applies its permission argument when it CREATES the file, so
// rewriting an already-loose config would leave it loose with a new secret in it.
func TestSaveTightensAnExistingLooseFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("contexts:\n  a:\n    server: http://x\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Skip("umask prevented the 0644 seed")
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Contexts["a"].Auth.Token = "s3cr3t"
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o after Save, want 600 — the secret is world-readable", perm)
	}

	// The rename must not leave the temporary file behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Fatalf("Save left a temporary file behind: %s", e.Name())
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := Default()
	cfg.Contexts["prod"] = &Context{Server: "https://x.example", Auth: Auth{APIKey: "k"}}
	cfg.CurrentContext = "prod"

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != "prod" || got.Contexts["prod"].Auth.APIKey != "k" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

// A YAML string is just a string: nothing expands ~ or resolves a relative path
// for us, so a token-file next to the config must still be found.
func TestPathsAreResolved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "prod.token")
	if err := os.WriteFile(tokenPath, []byte("  tok-123\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("contexts:\n  a:\n    server: http://x\n    auth:\n      token-file: prod.token\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Contexts["a"].Auth.TokenFile; got != tokenPath {
		t.Fatalf("token-file = %q, want it resolved against the config dir", got)
	}

	_, token, err := cfg.Contexts["a"].Credential()
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if token != "tok-123" {
		t.Fatalf("token = %q, want the trimmed file contents", token)
	}
}

func TestCredentialMissingTokenFile(t *testing.T) {
	t.Parallel()
	ctx := &Context{Server: "http://x", Auth: Auth{TokenFile: filepath.Join(t.TempDir(), "absent")}}
	if _, _, err := ctx.Credential(); err == nil {
		t.Fatal("a missing token-file must be an error")
	}
}

func TestDefaultPathHonoursEnvironment(t *testing.T) {
	t.Setenv("METRICSCTL_CONFIG", "/custom/config.yaml")
	if got := DefaultPath(); got != "/custom/config.yaml" {
		t.Fatalf("DefaultPath = %q", got)
	}

	t.Setenv("METRICSCTL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := DefaultPath(); got != filepath.Join("/xdg", "metricsctl", "config.yaml") {
		t.Fatalf("DefaultPath = %q, want the XDG location", got)
	}
}
