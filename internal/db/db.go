// Package db is kraai's Postgres touchpoint: connecting to a provisioned
// database, waiting for it to accept connections, and running the statements
// the provisioning flow needs.
//
// A driver rather than a psql subprocess: nothing spawned has a command
// line for another process to read a credential from, errors come from a
// library that does not print connection details, the binary needs no psql
// installed, and statements bind values rather than interpolating them.
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
// The role is a bound parameter: a role name is configuration, and a
// statement that could be steered into returning false would report safety
// it never verified.
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
