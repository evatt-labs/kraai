// Package db is kraai's Postgres touchpoint: connecting to a provisioned
// database, waiting for it to accept connections, and running the statements
// the provisioning flow needs.
//
// # Why a driver rather than psql
//
// The JavaScript this replaces shelled out to psql, which forced two pieces
// of defensive design on it: connection details had to travel in PG*
// environment variables rather than argv, because an argument holding a live
// credential is readable by any local process through /proc/<pid>/cmdline;
// and psql's stderr had to be discarded wholesale, because it echoes
// connection details back.
//
// A driver removes the subprocess and both problems with it. Nothing is
// spawned, so nothing has a command line to read, and errors come from a
// library that does not print credentials rather than from a CLI that does.
// It also makes the binary self-contained, which shelling out did not: a
// released kraai cannot assume psql is installed, and it frequently is not.
//
// The larger gain is parameterization. Statements now bind values instead of
// interpolating them, which removes an entire class of bug by construction
// rather than by remembering to quote — see AssertNoBypassRLS, whose
// JavaScript original interpolated a configured role straight into the
// row-level-security check.
package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/evatt-labs/kraai/internal/kerrors"
)

// Row is one result row. It is the single method this package needs from a
// query result, defined here rather than imported so the driver stays out of
// the surface every caller and test sees.
type Row interface {
	Scan(dest ...any) error
}

// Querier is an open connection to a provisioned database.
//
// Narrow on purpose: it is the consumer's interface, not the driver's, so a
// fake in a test implements three methods rather than pgx's full surface, and
// swapping drivers changes one file.
type Querier interface {
	// QueryRow runs sql with the given bound arguments and returns its first
	// row. Arguments are bound by the driver, never interpolated.
	QueryRow(ctx context.Context, sql string, args ...any) Row
	// Exec runs a statement that returns no rows.
	Exec(ctx context.Context, sql string, args ...any) error
	// Ping reports whether the connection is usable.
	Ping(ctx context.Context) error
	// Close releases the connection. It is safe to call more than once.
	Close(ctx context.Context) error
}

// Connector opens connections. Injecting one is how tests avoid needing a
// live Postgres, and how a different driver would be substituted.
type Connector interface {
	Connect(ctx context.Context, info ConnectionInfo) (Querier, error)
}

// Option configures a Client.
type Option func(*Client)

// WithConnector substitutes the connector a Client dials with.
func WithConnector(c Connector) Option {
	return func(cl *Client) { cl.connector = c }
}

// WithRetryDelay sets how long WaitForConnectable waits between attempts.
func WithRetryDelay(d time.Duration) Option {
	return func(cl *Client) { cl.retryDelay = d }
}

// Client is the Postgres operations kraai's provisioning flow needs.
type Client struct {
	connector  Connector
	retryDelay time.Duration
}

// Defaults for a Client and for WaitForConnectable.
const (
	DefaultConnectAttempts = 15
	DefaultRetryDelay      = 2 * time.Second
)

// New builds a Client. With no options it dials with the pgx-backed
// connector.
func New(opts ...Option) *Client {
	c := &Client{connector: PgxConnector{}, retryDelay: DefaultRetryDelay}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WaitForConnectable polls until the database accepts a connection, or until
// attempts are exhausted or ctx is done. A non-positive attempts means
// DefaultConnectAttempts.
//
// A freshly provisioned database is not immediately connectable — the
// provider returns before the compute endpoint is actually serving — so this
// is the normal path, not an error path.
func (c *Client) WaitForConnectable(ctx context.Context, info ConnectionInfo, attempts int) error {
	if attempts <= 0 {
		attempts = DefaultConnectAttempts
	}
	var last error
	for i := range attempts {
		err := c.probe(ctx, info)
		if err == nil {
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected, "waiting for the database to accept connections")
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return kerrors.Wrap(ctx.Err(), kerrors.CodeUnexpected, "waiting for the database to accept connections")
		case <-time.After(c.retryDelay):
		}
	}
	// The last failure is included: a wrong password and a propagation delay
	// look identical after thirty seconds of waiting, and the connector's
	// error is already reduced to host:port/database with no credential in
	// it, so there is nothing to withhold.
	return kerrors.Wrap(last, kerrors.CodeValidation,
		"database did not become connectable within %d attempts", attempts)
}

// probe opens a connection, pings, and closes it.
func (c *Client) probe(ctx context.Context, info ConnectionInfo) error {
	conn, err := c.connector.Connect(ctx, info)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	return conn.Ping(ctx)
}

// AssertNoBypassRLS fails unless role has BYPASSRLS disabled.
//
// Defensive, not decorative: a Neon-created role inherits BYPASSRLS from
// neon_superuser membership by default, which silently defeats row-level
// security for that role.
//
// The role is a bound parameter. The JavaScript this replaces interpolated it
// straight into the statement while its own quoting helper sat unused in the
// same file, and a role name is configuration rather than a constant. That
// mattered more here than anywhere else in the package: this is the
// row-level-security check, so a statement that can be steered into returning
// false reports safety it never verified. Binding removes the possibility
// rather than guarding against it.
//
// A role that does not exist returns no rows, which fails closed.
func (c *Client) AssertNoBypassRLS(ctx context.Context, info ConnectionInfo, role string) error {
	conn, err := c.connector.Connect(ctx, info)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	var bypass bool
	err = conn.QueryRow(ctx, "select rolbypassrls from pg_roles where rolname = $1", role).Scan(&bypass)
	switch {
	case errors.Is(err, ErrNoRows):
		return kerrors.Validation(
			"role %q does not exist — refusing to continue rather than assume row-level security is intact", role)
	case err != nil:
		return kerrors.Wrap(err, kerrors.CodeUnexpected, "checking BYPASSRLS for role %q", role)
	case bypass:
		return kerrors.Validation(
			"role %q has BYPASSRLS enabled — refusing to continue, row-level security would be "+
				"silently defeated for this role", role)
	}
	return nil
}

// Exec runs a statement, binding args rather than interpolating them.
func (c *Client) Exec(ctx context.Context, info ConnectionInfo, sql string, args ...any) error {
	conn, err := c.connector.Connect(ctx, info)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	return conn.Exec(ctx, sql, args...)
}

// QuoteLiteral renders value as a Postgres string literal the way the
// server's quote_literal() does: wrapped in single quotes, any single quote
// inside doubled.
//
// A last resort, not the normal path. Bind parameters wherever the statement
// allows them — which is anywhere a value appears. This exists for the places
// the protocol does not: an identifier or a DDL fragment cannot be bound, and
// building one by concatenation without quoting is how injection gets in.
func QuoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
