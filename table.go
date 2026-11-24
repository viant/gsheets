package gsheets

import (
	"errors"
	"fmt"
	"slices"
	"strconv"

	sqlite "github.com/ncruces/go-sqlite3"
)

type table struct {
	c *conn
	t *sheet
}
type cursor struct {
	t    *table
	rows [][]any
	pos  int
}

func (t *table) BestIndex(i *sqlite.IndexInfo) error {
	i.EstimatedCost = 10000
	i.EstimatedRows = 1000
	return nil
}
func (t *table) Open() (sqlite.VTabCursor, error) { return &cursor{t: t}, nil }
func (k *cursor) Filter(_ int, _ string, _ ...sqlite.Value) error {
	rows, err := k.t.c.load(k.t.t)
	if err != nil {
		return k.t.c.fail(err)
	}
	k.rows = rows
	k.pos = -1
	return k.Next()
}
func (k *cursor) Next() error {
	for k.pos++; k.pos < len(k.rows); k.pos++ {
		if slices.ContainsFunc(k.rows[k.pos], func(v any) bool { return v != nil }) {
			break
		}
	}
	return nil
}
func (k *cursor) EOF() bool             { return k.pos >= len(k.rows) }
func (k *cursor) RowID() (int64, error) { return int64(k.pos + 2), nil }
func (k *cursor) Column(ctx sqlite.Context, n int) error {
	if ctx.VTabNoChange() {
		return nil
	}
	row := k.rows[k.pos]
	if n >= len(row) || row[n] == nil {
		ctx.ResultNull()
		return nil
	}
	switch v := row[n].(type) {
	case string:
		ctx.ResultText(v)
	case float64:
		ctx.ResultText(strconv.FormatFloat(v, 'f', -1, 64))
	case int64:
		ctx.ResultText(strconv.FormatInt(v, 10))
	case bool:
		if v {
			ctx.ResultText("TRUE")
		} else {
			ctx.ResultText("FALSE")
		}
	default:
		return k.t.c.fail(fmt.Errorf("gsheets: unexpected cell type %T", v))
	}
	return nil
}
func (c *conn) fail(err error) error { c.active.err = err; return err }

var errRowID = errors.New("gsheets: assigning or changing rowid is unsupported")
