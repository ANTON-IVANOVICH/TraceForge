// Package config loads and saves metricsctl's configuration: a set of named
// contexts (server endpoint + credentials), one of which is current. The model
// is kubectl's, because it solves the real problem — one binary talking to
// production and staging without editing anything between invocations.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the config schema version, so a future change can be detected
// instead of silently misread.
const APIVersion = "v1"

// Kind names the document, mirroring the kubeconfig convention.
const Kind = "Config"

// Config is the whole configuration file.
type Config struct {
	APIVersion     string              `yaml:"apiVersion"`
	Kind           string              `yaml:"kind"`
	CurrentContext string              `yaml:"current-context"`
	Contexts       map[string]*Context `yaml:"contexts"`
}

// Context is one addressable deployment.
type Context struct {
	Server   string `yaml:"server"`             // e.g. http://localhost:8080
	Insecure bool   `yaml:"insecure,omitempty"` // skip TLS verification (https only)
	CAFile   string `yaml:"ca-file,omitempty"`  // custom CA bundle
	Auth     Auth   `yaml:"auth,omitempty"`
}

// Auth carries the credential used against a context. Exactly one of APIKey,
// Token or TokenFile is used, in that order.
type Auth struct {
	APIKey    string `yaml:"api-key,omitempty"`
	Token     string `yaml:"token,omitempty"`
	TokenFile string `yaml:"token-file,omitempty"`
}

// DefaultServer is what a fresh config points at.
const DefaultServer = "http://localhost:8080"

// Default returns an in-memory config for someone who has never run
// `metricsctl config set-context` — a local server, no credentials.
func Default() *Config {
	return &Config{
		APIVersion:     APIVersion,
		Kind:           Kind,
		CurrentContext: "default",
		Contexts: map[string]*Context{
			"default": {Server: DefaultServer},
		},
	}
}

// DefaultPath resolves where the config lives, honouring METRICSCTL_CONFIG and
// then the XDG base directory spec, and falling back to ~/.metricsctl.
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("METRICSCTL_CONFIG")); p != "" {
		return p
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "metricsctl", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".metricsctl", "config.yaml")
	}
	return filepath.Join(home, ".metricsctl", "config.yaml")
}

// Load reads the config at path (DefaultPath when empty). A missing file is not
// an error: it yields the default config, so the tool works out of the box and
// the first `config set-context` creates the file.
//
// ${VAR} and $VAR are expanded from the environment before parsing, which lets a
// checked-in config carry `token: ${METRICSCTL_TOKEN}` instead of a secret.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal([]byte(expandEnv(string(data))), &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	cfg.resolvePaths(filepath.Dir(path))
	return &cfg, nil
}

// Save writes the config with 0600 permissions — it holds credentials, and a
// world-readable one is the same mistake as a world-readable private key.
//
// It writes a fresh temporary file and renames it over the target. os.WriteFile
// would not do: its permission argument only applies when the file is *created*,
// so rewriting an already-loose config would leave it loose with a new secret
// inside. The rename is also atomic, so a crash mid-write cannot truncate the
// config.
func (c *Config) Save(path string) error {
	if path == "" {
		path = DefaultPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install config %s: %w", path, err)
	}
	return nil
}

// envRef matches a `${NAME}` placeholder.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv substitutes `${NAME}` from the environment, and nothing else.
//
// os.ExpandEnv would also expand the bare `$NAME` form, which silently mangles
// any credential containing a dollar sign: `S3cr$t!` becomes `S3cr!`, and the
// truncated secret is then sent to the server with no hint that it was rewritten.
// A placeholder whose variable is unset is left verbatim, so the failure is
// visible rather than an empty credential.
func expandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return match
	})
}

// Validate checks the structural invariants the rest of the CLI relies on.
func (c *Config) Validate() error {
	if c.APIVersion != "" && c.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", c.APIVersion, APIVersion)
	}
	if len(c.Contexts) == 0 {
		return errors.New("no contexts defined")
	}
	for name, ctx := range c.Contexts {
		if ctx == nil {
			return fmt.Errorf("context %q is empty", name)
		}
		if strings.TrimSpace(ctx.Server) == "" {
			return fmt.Errorf("context %q has no server", name)
		}
		// One credential per context. Two would both be sent, which is at best
		// confusing and at worst leaks the unused one to the server.
		if ctx.Auth.credentialCount() > 1 {
			return fmt.Errorf("context %q sets more than one of api-key, token and token-file", name)
		}
	}
	if c.CurrentContext != "" {
		if _, ok := c.Contexts[c.CurrentContext]; !ok {
			return fmt.Errorf("current-context %q is not defined", c.CurrentContext)
		}
	}
	return nil
}

// Resolve returns the context to use: the named one, else the current one.
func (c *Config) Resolve(name string) (string, *Context, error) {
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return "", nil, errors.New(`no current context; run "metricsctl config use-context <name>"`)
	}
	ctx, ok := c.Contexts[name]
	if !ok {
		return "", nil, fmt.Errorf(`context %q not found; run "metricsctl config get-contexts"`, name)
	}
	return name, ctx, nil
}

// Names returns the context names in a deterministic order.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.Contexts))
	for name := range c.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolvePaths expands `~` and makes file references relative to the config's
// own directory. A YAML string is just a string: nothing expands `~` for us, and
// a bare relative path would otherwise resolve against the caller's cwd.
func (c *Config) resolvePaths(base string) {
	for _, ctx := range c.Contexts {
		ctx.CAFile = resolvePath(ctx.CAFile, base)
		ctx.Auth.TokenFile = resolvePath(ctx.Auth.TokenFile, base)
	}
}

func resolvePath(p, base string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~"+string(os.PathSeparator)) || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// credentialCount reports how many credentials the auth block sets.
func (a Auth) credentialCount() int {
	var n int
	for _, v := range []string{a.APIKey, a.Token, a.TokenFile} {
		if v != "" {
			n++
		}
	}
	return n
}

// Credential returns the bearer token or API key for a context, reading
// token-file when one is configured. At most one is ever non-empty; Validate
// rejects a context that configures more than one.
func (ctx *Context) Credential() (apiKey, token string, err error) {
	if ctx.Auth.TokenFile != "" {
		data, err := os.ReadFile(ctx.Auth.TokenFile)
		if err != nil {
			return "", "", fmt.Errorf("read token-file: %w", err)
		}
		return "", strings.TrimSpace(string(data)), nil
	}
	return ctx.Auth.APIKey, ctx.Auth.Token, nil
}

// InsecurePermissions reports whether the config file is readable by anyone but
// its owner. Callers warn rather than fail, but they do warn: the file holds
// credentials.
func InsecurePermissions(path string) bool {
	if path == "" {
		path = DefaultPath()
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().Perm()&0o077 != 0
}
