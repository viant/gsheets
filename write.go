package gsheets

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"

	sqlite "github.com/ncruces/go-sqlite3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/sheets/v4"
)

// ErrConflict indicates that a row changed after it was read and before commit.
var ErrConflict = errors.New("gsheets: worksheet changed since it was read")

// CommitUnknownError means the batch may already be committed. Do not retry
// it blindly. A definite Sheets validation/authorization rejection is separate.
type CommitUnknownError struct{ Cause error }

func (e *CommitUnknownError) Error() string {
	return fmt.Sprintf("gsheets: batch commit outcome unknown: %v", e.Cause)
}
func (e *CommitUnknownError) Unwrap() error { return e.Cause }

type change struct {
	tab               *sheet
	index             int
	before            []any
	inserted, deleted bool
	fields            map[int]bool
}
type address struct {
	sheet int64
	row   int
}
type snapshot struct {
	data    map[int64][][]any
	changes map[address]change
	copied  map[int64]bool
	err     error
}

func newSnapshot() *snapshot {
	return &snapshot{data: map[int64][][]any{}, changes: map[address]change{}, copied: map[int64]bool{}}
}

// fork isolates a statement until every xUpdate has succeeded. Unchanged rows
// are immutable and shared, including transaction-local inserted rows.
func (s *snapshot) fork() *snapshot {
	n := newSnapshot()
	for k, v := range s.data {
		n.data[k] = v
	}
	for k, v := range s.changes {
		n.changes[k] = v
	}
	return n
}

func (t *table) Update(args ...sqlite.Value) (int64, error) {
	c := t.c
	if !c.cfg.ReadWrite || c.tx != nil && c.tx.readOnly {
		return 0, c.fail(errors.New("gsheets: read-only connection or transaction"))
	}
	rows, err := c.load(t.t)
	if err != nil {
		return 0, c.fail(err)
	}
	s := c.active
	if !s.copied[t.t.id] {
		rows = slices.Clone(rows)
		s.data[t.t.id] = rows
		s.copied[t.t.id] = true
	}
	insert := args[0].Type() == sqlite.NULL
	i := len(rows)
	if insert {
		if args[1].Type() != sqlite.NULL {
			return 0, c.fail(errRowID)
		}
	} else {
		id := args[0].Int64()
		if id < 2 || id-2 >= int64(len(rows)) {
			return 0, c.fail(ErrConflict)
		}
		i = int(id - 2)
		if len(args) > 1 && args[1].Int64() != id {
			return 0, c.fail(errRowID)
		}
	}
	key := address{t.t.id, i}
	ch, ok := s.changes[key]
	if !ok {
		ch = change{tab: t.t, index: i, inserted: insert}
		if !insert {
			ch.before = rows[i]
		}
	}
	fields := make(map[int]bool, len(ch.fields))
	for col := range ch.fields {
		fields[col] = true
	}
	ch.fields = fields
	if len(args) == 1 {
		rows[i] = nil
		ch.deleted = true
		s.changes[key] = ch
		return 0, nil
	}
	row := make([]any, len(t.t.columns))
	if !insert {
		copy(row, rows[i])
	}
	for j, v := range args[2:] {
		if v.NoChange() {
			continue
		}
		switch v.Type() {
		case sqlite.NULL:
			row[j] = nil
		case sqlite.TEXT:
			row[j] = v.Text()
		case sqlite.INTEGER:
			n := v.Int64()
			if n > 1<<53 || n < -(1<<53) {
				return 0, c.fail(errors.New("gsheets: integer exceeds exact Sheets numeric precision; bind as text"))
			}
			row[j] = n
		case sqlite.FLOAT:
			row[j] = v.Float()
		default:
			return 0, c.fail(errors.New("gsheets: unsupported binary cell value"))
		}
		fields[j] = true
	}
	if !slices.ContainsFunc(row, func(x any) bool { return x != nil }) {
		return 0, c.fail(errors.New("gsheets: empty rows cannot represent records"))
	}
	if insert {
		rows = append(rows, row)
	} else {
		rows[i] = row
	}
	s.data[t.t.id] = rows
	s.changes[key] = ch
	return int64(i + 2), nil
}

type transaction struct {
	c        *conn
	ctx      context.Context
	state    *snapshot
	readOnly bool
}

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }
func (c *conn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.tx != nil {
		return nil, errors.New("gsheets: transaction already open")
	}
	if o.Isolation != driver.IsolationLevel(sql.LevelDefault) {
		return nil, errors.New("gsheets: requested isolation is unsupported")
	}
	tx := &transaction{c: c, ctx: ctx, readOnly: o.ReadOnly || !c.cfg.ReadWrite, state: newSnapshot()}
	c.tx = tx
	return tx, nil
}
func (t *transaction) Rollback() error { t.c.tx = nil; return nil }
func (t *transaction) Commit() error {
	defer func() { t.c.tx = nil }()
	return t.c.commit(t.ctx, t.state)
}

func cell(v any) *sheets.CellData {
	e := &sheets.ExtendedValue{}
	switch x := v.(type) {
	case nil:
		return &sheets.CellData{}
	case string:
		e.StringValue = &x
	case float64:
		e.NumberValue = &x
	case int64:
		n := float64(x)
		e.NumberValue = &n
	case bool:
		e.BoolValue = &x
	}
	return &sheets.CellData{UserEnteredValue: e}
}
func update(tab int64, row, col int, values []any) *sheets.Request {
	data := &sheets.RowData{}
	for _, v := range values {
		data.Values = append(data.Values, cell(v))
	}
	return &sheets.Request{UpdateCells: &sheets.UpdateCellsRequest{
		Start:  &sheets.GridCoordinate{SheetId: tab, ForceSendFields: []string{"SheetId"}, RowIndex: int64(row + 1), ColumnIndex: int64(col)},
		Fields: "userEnteredValue", Rows: []*sheets.RowData{data},
	}}
}

func (c *conn) commit(ctx context.Context, s *snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(s.changes) == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.writer:
	}
	defer func() { c.writer <- struct{}{} }()
	keys := make([]address, 0, len(s.changes))
	for k, ch := range s.changes {
		if !(ch.inserted && ch.deleted) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sheet == keys[j].sheet {
			return keys[i].row < keys[j].row
		}
		return keys[i].sheet < keys[j].sheet
	})
	if err := c.verify(ctx, s, keys); err != nil {
		return err
	}
	var requests []*sheets.Request
	var appendRows *sheets.AppendCellsRequest
	for _, k := range keys {
		ch := s.changes[k]
		row := s.data[k.sheet][k.row]
		switch {
		case ch.inserted:
			if appendRows == nil || appendRows.SheetId != k.sheet {
				appendRows = &sheets.AppendCellsRequest{SheetId: k.sheet, ForceSendFields: []string{"SheetId"}, Fields: "userEnteredValue"}
				requests = append(requests, &sheets.Request{AppendCells: appendRows})
			}
			r := &sheets.RowData{}
			for _, v := range row {
				r.Values = append(r.Values, cell(v))
			}
			appendRows.Rows = append(appendRows.Rows, r)
		case ch.deleted:
			requests = append(requests, update(k.sheet, k.row, 0, make([]any, len(ch.tab.columns))))
		default:
			for j := 0; j < len(row); {
				if !ch.fields[j] {
					j++
					continue
				}
				start := j
				for j < len(row) && ch.fields[j] {
					j++
				}
				requests = append(requests, update(k.sheet, k.row, start, row[start:j]))
			}
		}
	}
	if len(requests) == 0 {
		return nil
	}
	_, err := c.api.Spreadsheets.BatchUpdate(c.cfg.SpreadsheetID, &sheets.BatchUpdateSpreadsheetRequest{Requests: requests}).Context(ctx).Do()
	if err == nil {
		return nil
	}
	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) && apiErr.Code >= 400 && apiErr.Code < 500 && apiErr.Code != 408 {
		return fmt.Errorf("commit worksheet batch: %w", err)
	}
	return &CommitUnknownError{Cause: err}
}

// Recheck touched headers and preexisting rows in one request. Sheets has no
// compare-and-swap precondition spanning this check and the subsequent batch.
func (c *conn) verify(ctx context.Context, s *snapshot, keys []address) error {
	var filters []*sheets.DataFilter
	expect := map[address][]any{}
	for _, k := range keys {
		ch := s.changes[k]
		header := address{k.sheet, -1}
		if _, ok := expect[header]; !ok {
			// Use the same normalization as discovery, including numeric headers.
			expect[header] = nil
			filters = append(filters, byID(k.sheet, true))
		}
		if !ch.inserted {
			expect[k] = ch.before
			filters = append(filters, &sheets.DataFilter{GridRange: &sheets.GridRange{SheetId: k.sheet, ForceSendFields: []string{"SheetId"}, StartRowIndex: int64(k.row + 1), EndRowIndex: int64(k.row + 2)}})
		}
	}
	r, err := c.api.Spreadsheets.Values.BatchGetByDataFilter(c.cfg.SpreadsheetID, &sheets.BatchGetValuesByDataFilterRequest{DataFilters: filters, ValueRenderOption: "UNFORMATTED_VALUE"}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("verify pending edits: %w", err)
	}
	for _, v := range r.ValueRanges {
		if v.ValueRange == nil {
			continue
		}
		for _, f := range v.DataFilters {
			g := f.GridRange
			if g == nil {
				continue
			}
			k := address{g.SheetId, int(g.StartRowIndex) - 1}
			old, ok := expect[k]
			if !ok {
				continue
			}
			if k.row < 0 {
				if !slices.Equal(firstRow(v.ValueRange.Values), c.byID[k.sheet].header) {
					c.invalidate()
					return ErrSchemaChanged
				}
			} else if len(v.ValueRange.Values) != 1 || !reflect.DeepEqual(old, v.ValueRange.Values[0]) {
				return ErrConflict
			}
			delete(expect, k)
		}
	}
	if len(expect) > 0 {
		return ErrConflict
	}
	return nil
}
