package gsheets_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/viant/gsheets"
	_ "github.com/viant/gsheets/metadata"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type fixture struct {
	sync.Mutex
	api      *httptest.Server
	tabs     map[int64]string
	rows     map[int64][][]any
	calls    map[string]int
	batches  []*sheets.BatchUpdateSpreadsheetRequest
	reject   int
	failRead int
}

func newFixture(t testing.TB) *fixture {
	f := &fixture{tabs: map[int64]string{0: "Products", 42: "Costs"}, rows: map[int64][][]any{0: {{"sku", "name"}, {"a", "Apples"}, {"b", "Bananas"}}, 42: {{"sku", "cost"}, {"a", 1.5}, {"b", 3.25}}}, calls: map[string]int{}}
	f.api = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.api.Close)
	return f
}
func (f *fixture) open(t testing.TB, rw bool) *sql.DB {
	t.Helper()
	c, err := gsheets.NewConnector(gsheets.Config{SpreadsheetID: "book", ReadWrite: rw}, option.WithEndpoint(f.api.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.Lock()
	defer f.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/v4/spreadsheets/book")
	f.calls[path]++
	switch path {
	case "":
		book := sheets.Spreadsheet{}
		for id, name := range f.tabs {
			book.Sheets = append(book.Sheets, &sheets.Sheet{Properties: &sheets.SheetProperties{SheetId: id, Title: name, SheetType: "GRID"}})
		}
		json.NewEncoder(w).Encode(book)
	case "/values:batchGet":
		resp := sheets.BatchGetValuesResponse{}
		for _, name := range r.URL.Query()["ranges"] {
			var rows [][]any
			for id, title := range f.tabs {
				if strings.HasPrefix(name, "'"+title+"'") {
					rows = f.rows[id]
					if strings.HasSuffix(name, "!1:1") {
						rows = rows[:1]
					} else if f.failRead != 0 {
						http.Error(w, "unavailable", f.failRead)
						return
					}
				}
			}
			resp.ValueRanges = append(resp.ValueRanges, &sheets.ValueRange{Values: rows, Range: name})
		}
		json.NewEncoder(w).Encode(resp)
	case "/values:batchGetByDataFilter":
		var q sheets.BatchGetValuesByDataFilterRequest
		json.NewDecoder(r.Body).Decode(&q)
		resp := sheets.BatchGetValuesByDataFilterResponse{}
		for _, filt := range q.DataFilters {
			g := filt.GridRange
			v := f.rows[g.SheetId]
			start := min(len(v), int(g.StartRowIndex))
			end := len(v)
			if g.EndRowIndex > 0 {
				end = min(end, int(g.EndRowIndex))
			}
			resp.ValueRanges = append(resp.ValueRanges, &sheets.MatchedValueRange{DataFilters: []*sheets.DataFilter{filt}, ValueRange: &sheets.ValueRange{Values: v[start:end]}})
		}
		json.NewEncoder(w).Encode(resp)
	case ":batchUpdate":
		var batch sheets.BatchUpdateSpreadsheetRequest
		json.NewDecoder(r.Body).Decode(&batch)
		f.batches = append(f.batches, &batch)
		if f.reject != 0 {
			http.Error(w, "batch rejected", f.reject)
			return
		}
		for _, r := range batch.Requests {
			if a := r.AppendCells; a != nil {
				for _, rd := range a.Rows {
					f.rows[a.SheetId] = append(f.rows[a.SheetId], cells(rd))
				}
			}
			if u := r.UpdateCells; u != nil {
				row := f.rows[u.Start.SheetId][u.Start.RowIndex]
				for j, v := range cells(u.Rows[0]) {
					col := int(u.Start.ColumnIndex) + j
					for len(row) <= col {
						row = append(row, nil)
					}
					row[col] = v
				}
				f.rows[u.Start.SheetId][u.Start.RowIndex] = row
			}
		}
		io.WriteString(w, "{}")
	default:
		http.NotFound(w, r)
	}
}
func cells(rd *sheets.RowData) []any {
	out := make([]any, len(rd.Values))
	for i, c := range rd.Values {
		if c == nil || c.UserEnteredValue == nil {
			continue
		}
		e := c.UserEnteredValue
		switch {
		case e.StringValue != nil:
			out[i] = *e.StringValue
		case e.NumberValue != nil:
			out[i] = *e.NumberValue
		case e.BoolValue != nil:
			out[i] = *e.BoolValue
		}
	}
	return out
}
func TestReadSQL(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, false)
	st, err := db.Prepare("SELECT p.name, CAST(c.cost AS REAL) AS price FROM Products p JOIN Costs c ON c.sku=p.sku WHERE p.sku=:sku")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 2; i++ {
		var name string
		var price float64
		if err := st.QueryRow(sql.Named("sku", "b")).Scan(&name, &price); err != nil {
			t.Fatal(err)
		}
		if name != "Bananas" || price != 3.25 {
			t.Fatal(name, price)
		}
	}
	if f.calls[""] != 1 || f.calls["/values:batchGet"] != 5 {
		t.Fatalf("want one metadata read, one header batch and two tabs per query: %v", f.calls)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM Products a JOIN Products b ON a.sku=b.sku").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if f.calls["/values:batchGet"] != 6 {
		t.Fatalf("self join fetched tab more than once: %v", f.calls)
	}
	if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('Products')").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if f.calls["/values:batchGet"] != 6 {
		t.Fatal("metadata read loaded sheet data")
	}
	for _, q := range []string{"UPDATE Products SET name='x'", "ATTACH ':memory:' AS other", "PRAGMA writable_schema=ON", "DROP TABLE Products", "SELECT 1; DELETE FROM Products"} {
		if _, err := db.Exec(q); err == nil {
			t.Fatal("accepted", q)
		}
	}
	if len(f.batches) != 0 {
		t.Fatal("sent write in read-only mode")
	}
}
func TestAtomicWrite(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, true)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		r, err := tx.Exec(q, args...)
		if err != nil {
			t.Fatal(q, err)
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			t.Fatal(q, n, err)
		}
	}
	exec("UPDATE Products SET name=? WHERE sku=?", "Pears", "a")
	exec("DELETE FROM Costs WHERE sku='b'")
	exec("INSERT INTO Costs(sku,cost) VALUES('c','9')")
	exec("UPDATE Costs SET cost='8' WHERE sku='c'")
	var v string
	if err := tx.QueryRow("SELECT cost FROM Costs WHERE sku='c'").Scan(&v); err != nil || v != "8" {
		t.Fatal(v, err)
	}
	if len(f.batches) != 0 {
		t.Fatal("write escaped before Commit")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 1 || len(f.batches[0].Requests) != 3 {
		t.Fatalf("expected a single batch for two tabs: %#v", f.batches)
	}
	if !reflect.DeepEqual(f.rows[0][1], []any{"a", "Pears"}) || !reflect.DeepEqual(f.rows[42][3], []any{"c", "8"}) {
		t.Fatal(f.rows)
	}
	if err := db.QueryRow("SELECT count(*) FROM Costs WHERE sku='b'").Scan(&v); err != nil || v != "0" {
		t.Fatal(v, err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO Products VALUES('d','=literal')")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 1 {
		t.Fatal("rollback sent a write")
	}
}
func TestWriteErrors(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, true)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE Products SET name='mine' WHERE sku='a'"); err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.rows[0][1][1] = "other editor"
	f.Unlock()
	if err = tx.Commit(); !errors.Is(err, gsheets.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if len(f.batches) != 0 {
		t.Fatal("sent conflicted batch")
	}
	for _, code := range []int{400, 403, 503} {
		f.Lock()
		f.reject = code
		f.Unlock()
		_, err := db.Exec("INSERT INTO Products VALUES('c','Cherry')")
		var cause *googleapi.Error
		var unknown *gsheets.CommitUnknownError
		if !errors.As(err, &cause) || cause.Code != code || errors.As(err, &unknown) != (code == 503) {
			t.Fatal(code, err)
		}
	}
	if len(f.batches) != 3 {
		t.Fatalf("unexpected retries: %d", len(f.batches))
	}
}
func TestRowsErrors(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, false)
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	f.Lock()
	f.failRead = 403
	f.Unlock()
	var n int
	err := db.QueryRow("SELECT count(*) FROM Products").Scan(&n)
	var api *googleapi.Error
	if !errors.As(err, &api) || api.Code != 403 {
		t.Fatalf("source error lost: %v", err)
	}
	f.Lock()
	f.failRead = 0
	f.rows[0][0][1] = "changed"
	f.Unlock()
	if err = db.QueryRow("SELECT count(*) FROM Products").Scan(&n); !errors.Is(err, gsheets.ErrSchemaChanged) {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT count(changed) FROM Products").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(fmt.Sprint(err))
	}
}

func TestFailedStatementDiscardsPartialChanges(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, true)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO Products VALUES ('c','Cherry')"); err != nil {
		t.Fatal(err)
	}
	// First row is valid; changing the rowid of the second row is forbidden.
	if _, err = tx.Exec("UPDATE Products SET name='partial', rowid=CASE WHEN sku='b' THEN 99 ELSE rowid END"); err == nil {
		t.Fatal("unexpected success")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(f.batches) != 1 || len(f.batches[0].Requests) != 1 || f.batches[0].Requests[0].AppendCells == nil {
		t.Fatal("failed statement leaked changes", f.batches)
	}
	if got := f.rows[0][1][1]; got != "Apples" {
		t.Fatal(got)
	}
}

func TestPooledMetadataAndConflictingTransactions(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, true)
	db.SetMaxOpenConns(2)
	first, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	second, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	if f.calls[""] != 1 || f.calls["/values:batchGetByDataFilter"] != 1 {
		t.Fatalf("duplicated catalog requests: %v", f.calls)
	}
	for i, tx := range []*sql.Tx{first, second} {
		if _, err = tx.Exec("UPDATE Products SET name=? WHERE sku='a'", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	errs := make(chan error, 2)
	go func() { errs <- first.Commit() }()
	go func() { errs <- second.Commit() }()
	a, b := <-errs, <-errs
	if !(a == nil && errors.Is(b, gsheets.ErrConflict) || b == nil && errors.Is(a, gsheets.ErrConflict)) {
		t.Fatalf("want commit + conflict, got %v and %v", a, b)
	}
	if len(f.batches) != 1 {
		t.Fatal("expected one commit", len(f.batches))
	}
}

func TestReadOnlyTransactionOnWritableConnection(t *testing.T) {
	f := newFixture(t)
	db := f.open(t, true)
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO Products VALUES('b','bad')"); err == nil {
		t.Fatal("read-only write")
	}
	if len(f.batches) > 0 {
		t.Fatal("write escaped read-only transaction")
	}
}

func TestDSN(t *testing.T) {
	c, err := gsheets.ParseDSN("gsheets://book?tab=North+America&mode=rw&credURL=%2Fsecrets%2Fgoogle.json")
	if err != nil || !c.ReadWrite || c.SpreadsheetID != "book" || c.CredURL != "/secrets/google.json" || !reflect.DeepEqual(c.Tabs, []string{"North America"}) {
		t.Fatal(c, err)
	}
	for _, dsn := range []string{"gsheets://book?mode=write", "gsheets://book?credKey=orphan", "gsheets://book?oauth2TokenURL=t", "gsheets://book?credURL=x&credID=y", "gsheets://book?tab=X&tab=x", "gsheets://me@book", "gsheets://book?tab=%ZZ", "gsheets://book?user=someone"} {
		if _, err := gsheets.ParseDSN(dsn); err == nil {
			t.Fatal("accepted invalid DSN", dsn)
		}
	}
}

func BenchmarkQuery(b *testing.B) {
	for _, cold := range []bool{false, true} {
		b.Run(map[bool]string{false: "pooled", true: "new_connector"}[cold], func(b *testing.B) {
			f := newFixture(b)
			for i := 0; i < 1000; i++ {
				f.rows[0] = append(f.rows[0], []any{fmt.Sprint(i), "Product"})
			}
			newDB := func() *sql.DB {
				c, err := gsheets.NewConnector(gsheets.Config{SpreadsheetID: "book"}, option.WithEndpoint(f.api.URL+"/"), option.WithoutAuthentication())
				if err != nil {
					b.Fatal(err)
				}
				return sql.OpenDB(c)
			}
			query := func(db *sql.DB) {
				var count int
				if err := db.QueryRow("SELECT COUNT(*) FROM Products WHERE name=?", "Product").Scan(&count); err != nil || count != 1000 {
					b.Fatal(count, err)
				}
			}
			db := newDB()
			defer db.Close()
			query(db)
			f.Lock()
			f.calls = map[string]int{}
			f.Unlock()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if cold {
					db := newDB()
					query(db)
					db.Close()
				} else {
					query(db)
				}
			}
			b.StopTimer()
			f.Lock()
			defer f.Unlock()
			b.ReportMetric(float64(f.calls[""]+f.calls["/values:batchGet"])/float64(b.N), "api_calls/op")
		})
	}
}
