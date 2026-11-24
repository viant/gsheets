package gsheets_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/viant/gsheets"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// This optional test writes only to two newly created tabs in the explicitly
// supplied test workbook. No existing tab may be used as a fixture.
func TestLiveAtomicWrite(t *testing.T) {
	id := os.Getenv("GSHEETS_TEST_RW_ID")
	if id == "" {
		t.Skip("set GSHEETS_TEST_RW_ID to an editable, disposable native Google spreadsheet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api, err := sheets.NewService(ctx, option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		t.Fatal(err)
	}
	names := [2]string{fmt.Sprintf("gsheets_test_%d_a", time.Now().UnixNano()), fmt.Sprintf("gsheets_test_%d_b", time.Now().UnixNano())}
	var add []*sheets.Request
	for _, n := range names {
		add = append(add, &sheets.Request{AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: n}}})
	}
	created, err := api.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{Requests: add}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	var cleanup, header []*sheets.Request
	for _, reply := range created.Replies {
		sid := reply.AddSheet.Properties.SheetId
		cleanup = append(cleanup, &sheets.Request{DeleteSheet: &sheets.DeleteSheetRequest{SheetId: sid}})
		title := "label"
		header = append(header, &sheets.Request{UpdateCells: &sheets.UpdateCellsRequest{Start: &sheets.GridCoordinate{SheetId: sid}, Fields: "userEnteredValue", Rows: []*sheets.RowData{{Values: []*sheets.CellData{{UserEnteredValue: &sheets.ExtendedValue{StringValue: &title}}}}}}})
	}
	t.Cleanup(func() {
		finish, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, e := api.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{Requests: cleanup}).Context(finish).Do(); e != nil {
			t.Errorf("remove fixture tabs %v: %v", names, e)
		}
	})
	if _, err = api.Spreadsheets.BatchUpdate(id, &sheets.BatchUpdateSpreadsheetRequest{Requests: header}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	opts := url.Values{"mode": {"rw"}, "tab": names[:]}
	db, err := sql.Open("gsheets", "gsheets://"+id+"?"+opts.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, name := range names {
		if _, err = tx.ExecContext(ctx, "INSERT INTO "+name+"(label) VALUES (?)", "first"); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE "+names[0]+" SET label=? WHERE label=?", "second", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM "+names[1]+" WHERE label=?", "first"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var got string
	if err = db.QueryRowContext(ctx, "SELECT label FROM "+names[0]).Scan(&got); err != nil || got != "second" {
		t.Fatal(got, err)
	}
	if err = db.QueryRowContext(ctx, "SELECT label FROM "+names[1]).Scan(&got); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
}
