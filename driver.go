package gsheets

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	sqlite "github.com/ncruces/go-sqlite3"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// DriverName is the name registered with database/sql.
const DriverName = "gsheets"

func init() { sql.Register(DriverName, Driver{}) }

// Driver implements driver.DriverContext for Google Sheets.
type Driver struct{}

func (Driver) Open(dsn string) (driver.Conn, error) {
	c, err := (Driver{}).OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}
func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	c, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return NewConnector(c)
}

// Connector owns the catalog and credentials shared by connections in one pool.
type Connector struct {
	config         Config
	options        []option.ClientOption
	writer         chan struct{}
	catalogLock    chan struct{}
	revision       atomic.Uint64
	api            *sheets.Service
	schema         []sheet
	schemaRevision uint64
	schemaUntil    time.Time
}

// NewConnector creates a connector for sql.OpenDB. Google API client options
// apply only to connections created by this connector.
func NewConnector(cfg Config, opts ...option.ClientOption) (*Connector, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.Tabs = slices.Clone(cfg.Tabs)
	writer := make(chan struct{}, 1)
	writer <- struct{}{}
	return &Connector{config: cfg, options: slices.Clone(opts), writer: writer, catalogLock: make(chan struct{}, 1)}, nil
}
func (*Connector) Driver() driver.Driver { return Driver{} }
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	api, tabs, revision, until, err := c.describe(ctx)
	if err != nil {
		return nil, err
	}
	db, err := sqlite.OpenContext(ctx, ":memory:")
	if err != nil {
		return nil, err
	}
	x := &conn{owner: c, revision: revision, until: until, db: db, api: api, cfg: c.config, writer: c.writer, byID: map[int64]*sheet{}, tables: make(map[string]*sheet, len(tabs))}
	if err = x.install(tabs); err != nil {
		_ = db.Close()
		return nil, err
	}
	return x, nil
}

// RefreshSchema invalidates this pool's tab and header snapshot. Checked-out
// queries continue with their original schema. New checkouts rediscover it.
func (c *Connector) RefreshSchema() { c.revision.Add(1) }

func (c *Connector) describe(ctx context.Context) (*sheets.Service, []sheet, uint64, time.Time, error) {
	select {
	case c.catalogLock <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, 0, time.Time{}, ctx.Err()
	}
	defer func() { <-c.catalogLock }()
	rev := c.revision.Load()
	if c.api == nil {
		api, err := c.config.service(ctx, c.options)
		if err != nil {
			return nil, nil, 0, time.Time{}, err
		}
		c.api = api
	}
	if rev != c.schemaRevision || !time.Now().Before(c.schemaUntil) {
		tabs, err := catalog(ctx, c.api, c.config)
		if err != nil {
			return nil, nil, 0, time.Time{}, err
		}
		c.schema, c.schemaRevision, c.schemaUntil = tabs, rev, time.Now().Add(time.Minute)
	}
	return c.api, c.schema, rev, c.schemaUntil, nil
}

type conn struct {
	owner    *Connector
	revision uint64
	until    time.Time
	db       *sqlite.Conn
	api      *sheets.Service
	cfg      Config
	tables   map[string]*sheet
	writer   chan struct{}
	byID     map[int64]*sheet
	active   *snapshot
	tx       *transaction
	stale    bool
}

func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (c *conn) install(tabs []sheet) error {
	const module = "gsheets_grid"
	open := func(db *sqlite.Conn, _, _, name string, _ ...string) (*table, error) {
		t := c.tables[name]
		if t == nil {
			return nil, errors.New("gsheets: unknown worksheet")
		}
		columns := make([]string, len(t.columns))
		for i, s := range t.columns {
			columns[i] = ident(s) + " TEXT"
		}
		err := db.DeclareVTab("CREATE TABLE x(" + strings.Join(columns, ",") + ")")
		return &table{c, t}, err
	}
	if err := sqlite.CreateModule(c.db, module, open, open); err != nil {
		return err
	}
	for i := range tabs {
		t := &tabs[i]
		c.tables[t.title] = t
		c.byID[t.id] = t
		if strings.ContainsRune(t.title, 0) {
			return errors.New("gsheets: NUL in worksheet name")
		}
		if err := c.db.Exec("CREATE VIRTUAL TABLE " + ident(t.title) + " USING " + module); err != nil {
			return fmt.Errorf("mount tab %q: %w", t.title, err)
		}
	}
	return c.db.SetAuthorizer(c.authorize)
}
func (c *conn) authorize(action sqlite.AuthorizerActionCode, name, arg, _, _ string) sqlite.AuthorizerReturnCode {
	switch action {
	case sqlite.AUTH_SELECT, sqlite.AUTH_READ, sqlite.AUTH_RECURSIVE, sqlite.AUTH_FUNCTION:
		return sqlite.AUTH_OK
	case sqlite.AUTH_PRAGMA:
		switch strings.ToLower(name) {
		case "database_list", "table_info", "table_xinfo", "table_list", "index_list", "index_info", "index_xinfo", "foreign_key_list", "collation_list":
			return sqlite.AUTH_OK
		}
	case sqlite.AUTH_INSERT, sqlite.AUTH_DELETE, sqlite.AUTH_UPDATE:
		if c.cfg.ReadWrite && c.tables[name] != nil && (c.tx == nil || !c.tx.readOnly) {
			return sqlite.AUTH_OK
		}
	}
	return sqlite.AUTH_DENY
}
func (c *conn) Close() error { return c.db.Close() }
func (c *conn) IsValid() bool {
	return !c.stale && c.revision == c.owner.revision.Load() && time.Now().Before(c.until)
}
func (c *conn) ResetSession(ctx context.Context) error {
	if !c.IsValid() {
		return driver.ErrBadConn
	}
	return ctx.Err()
}
func (c *conn) invalidate() {
	c.stale = true
	c.owner.revision.CompareAndSwap(c.revision, c.revision+1)
}

func (c *conn) Ping(ctx context.Context) error {
	_, err := c.api.Spreadsheets.Get(c.cfg.SpreadsheetID).Fields("spreadsheetId").Context(ctx).Do()
	return err
}
