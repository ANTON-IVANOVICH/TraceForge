package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"metrics-system/internal/cli/client"
	"metrics-system/internal/cli/config"
	"metrics-system/internal/cli/output"
)

// Context is the dependency bundle every command needs: the resolved
// configuration, a lazily-built API client, a printer and the three streams.
//
// Streams are fields rather than os.Stdout/os.Stderr because a command that
// writes to the global streams cannot be tested. Every command writes here.
type Context struct {
	Config      *config.Config
	ConfigPath  string
	ContextName string
	Server      *config.Context

	Printer   output.Printer
	Color     *output.Colorizer
	Stdout    io.Writer
	Stderr    io.Writer
	Stdin     io.Reader
	Timeout   time.Duration
	Verbose   bool
	AssumeYes bool

	// newClient builds the API client on first use, so commands that never talk
	// to a server (config, completion, version) work without one.
	once   sync.Once
	client *client.Client
	err    error
}

// Client returns the API client, building it once.
func (c *Context) Client() (*client.Client, error) {
	c.once.Do(func() {
		c.client, c.err = client.New(c.Server, c.Timeout)
	})
	return c.client, c.err
}

// SetClient injects a client, for tests.
func (c *Context) SetClient(cl *client.Client) {
	c.once.Do(func() { c.client = cl })
}

type ctxKey struct{}

// WithContext attaches the CLI context to a Go context.
func WithContext(parent context.Context, c *Context) context.Context {
	return context.WithValue(parent, ctxKey{}, c)
}

// FromContext extracts the CLI context, or nil.
func FromContext(ctx context.Context) *Context {
	c, _ := ctx.Value(ctxKey{}).(*Context)
	return c
}

// MustFromContext extracts the CLI context, panicking when it is absent —
// which can only mean the root command's PersistentPreRunE did not run, i.e. a
// wiring bug rather than a user error.
func MustFromContext(ctx context.Context) *Context {
	c := FromContext(ctx)
	if c == nil {
		panic("cli: context not initialised (PersistentPreRunE did not run)")
	}
	return c
}

// Debugf prints to stderr only in verbose mode.
func (c *Context) Debugf(format string, args ...any) {
	if !c.Verbose || c.Stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(c.Stderr, "["+c.ContextName+"] "+format+"\n", args...)
}
