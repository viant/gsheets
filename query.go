package gsheets

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	sqlite "github.com/ncruces/go-sqlite3"
)

type stmt struct {
	c      *conn
	query  string
	idle   *sqlite.Stmt
	count  int
	closed bool
}

func (c *conn) compile(ctx context.Context, query string) (*sqlite.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prev := c.db.SetInterrupt(ctx)
	defer c.db.SetInterrupt(prev)
	s, tail, err := c.db.Prepare(query)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("gsheets: expected a SQL statement")
	}
	if strings.TrimSpace(tail) != "" {
		_ = s.Close()
		return nil, errors.New("gsheets: multiple statements are unsupported")
	}
	return s, nil
}
func (c *conn) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}
func (c *conn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	s, err := c.compile(ctx, q)
	if err != nil {
		return nil, err
	}
	return &stmt{c: c, query: q, idle: s, count: s.BindCount()}, nil
}
func (s *stmt) Close() error {
	s.closed = true
	if s.idle != nil {
		err := s.idle.Close()
		s.idle = nil
		return err
	}
	return nil
}
func (s *stmt) NumInput() int { return s.count }
func (s *stmt) take(ctx context.Context, args []driver.NamedValue) (*sqlite.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, errors.New("gsheets: statement closed")
	}
	st := s.idle
	s.idle = nil
	if st == nil {
		var err error
		st, err = s.c.compile(ctx, s.query)
		if err != nil {
			return nil, err
		}
	}
	if err := bind(st, args); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}
func (s *stmt) release(st *sqlite.Stmt) error {
	err := st.Reset()
	if err == nil {
		err = st.ClearBindings()
	}
	if err != nil || s.closed || s.idle != nil {
		return errors.Join(err, st.Close())
	}
	s.idle = st
	return nil
}
func bind(st *sqlite.Stmt, args []driver.NamedValue) error {
	if len(args) != st.BindCount() {
		return fmt.Errorf("gsheets: expected %d bind arguments, got %d", st.BindCount(), len(args))
	}
	used := map[int]bool{}
	for _, a := range args {
		i := a.Ordinal
		if a.Name != "" {
			i = 0
			for _, prefix := range []string{":", "@", "$"} {
				if i = st.BindIndex(prefix + a.Name); i != 0 {
					break
				}
			}
			if i == 0 {
				return fmt.Errorf("gsheets: unknown named parameter %q", a.Name)
			}
		}
		if used[i] {
			return fmt.Errorf("gsheets: repeated argument %d", i)
		}
		used[i] = true
		var err error
		switch v := a.Value.(type) {
		case nil:
			err = st.BindNull(i)
		case int64:
			err = st.BindInt64(i, v)
		case float64:
			err = st.BindFloat(i, v)
		case string:
			err = st.BindText(i, v)
		case bool:
			err = st.BindBool(i, v)
		case []byte:
			err = st.BindBlob(i, v)
		case time.Time:
			err = st.BindText(i, v.Format(time.RFC3339Nano))
		default:
			return fmt.Errorf("gsheets: unsupported parameter %T", v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func values(args []driver.Value) []driver.NamedValue {
	result := make([]driver.NamedValue, len(args))
	for i, v := range args {
		result[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return result
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), values(args))
}
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), values(args))
}
func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	st, err := s.take(ctx, args)
	if err != nil {
		return nil, err
	}
	if !st.ReadOnly() {
		_ = s.release(st)
		return nil, errors.New("gsheets: use ExecContext for mutations; RETURNING is unsupported")
	}
	state := newSnapshot()
	if s.c.tx != nil {
		state = s.c.tx.state
	}
	cols := make([]string, st.ColumnCount())
	for i := range cols {
		cols[i] = st.ColumnName(i)
	}
	return &rows{owner: s, st: st, state: state, ctx: ctx, cols: cols}, nil
}
func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (result driver.Result, err error) {
	st, err := s.take(ctx, args)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := s.release(st); err == nil {
			err = e
		}
	}()
	c := s.c
	if st.ReadOnly() || st.ColumnCount() != 0 {
		return nil, errors.New("gsheets: expected mutation without RETURNING")
	}
	if !c.cfg.ReadWrite || c.tx != nil && c.tx.readOnly {
		return nil, errors.New("gsheets: write requires mode=rw")
	}
	state := newSnapshot()
	if c.tx != nil {
		state = c.tx.state.fork()
	}
	c.active = state
	defer func() { c.active = nil }()
	prev := c.db.SetInterrupt(ctx)
	defer c.db.SetInterrupt(prev)
	st.Step()
	if err = state.err; err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = st.Err(); err != nil {
		return nil, err
	}
	affected := driver.RowsAffected(c.db.Changes())
	if c.tx != nil {
		c.tx.state = state
	} else if err = c.commit(ctx, state); err != nil {
		return nil, err
	}
	return affected, nil
}

type rows struct {
	owner *stmt
	st    *sqlite.Stmt
	state *snapshot
	ctx   context.Context
	cols  []string
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error {
	if r.st == nil {
		return nil
	}
	err := r.owner.release(r.st)
	r.st = nil
	r.state = nil
	return err
}
func (r *rows) Next(dest []driver.Value) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	c := r.owner.c
	c.active = r.state
	defer func() { c.active = nil }()
	prev := c.db.SetInterrupt(r.ctx)
	defer c.db.SetInterrupt(prev)
	r.state.err = nil
	if !r.st.Step() {
		if err := r.state.err; err != nil {
			return err
		}
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if err := r.st.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	for i := range dest {
		switch r.st.ColumnType(i) {
		case sqlite.INTEGER:
			dest[i] = r.st.ColumnInt64(i)
		case sqlite.FLOAT:
			dest[i] = r.st.ColumnFloat(i)
		case sqlite.TEXT:
			dest[i] = r.st.ColumnText(i)
		case sqlite.BLOB:
			dest[i] = r.st.ColumnBlob(i, nil)
		default:
			dest[i] = nil
		}
	}
	return nil
}

func (r *rows) ColumnTypeDatabaseTypeName(i int) string { return r.st.ColumnDeclType(i) }
func (r *rows) ColumnTypeNullable(i int) (bool, bool)   { return true, r.st.ColumnTableName(i) != "" }
func (r *rows) ColumnTypeScanType(i int) reflect.Type {
	if r.st.ColumnDeclType(i) == "TEXT" {
		return reflect.TypeFor[string]()
	}
	return reflect.TypeFor[any]()
}
