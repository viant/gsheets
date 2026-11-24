# gsheets

A CGO-free Go `database/sql` driver for **native Google Sheets**. Connect to one spreadsheet, query its tabs as SQLite virtual tables, and optionally commit changes to multiple tabs in one atomic Google Sheets request.

## Quick start

Use Go 1.25.8 or later. Install the driver in your Go module:

```sh
go get github.com/viant/gsheets
```

Enable the Google Sheets API, configure Application Default Credentials, and grant that identity Viewer access to a native Google spreadsheet. Its ID is the part of the Sheets URL between `/d/` and `/edit`. For this example, create two tabs with headers in row 1:

| Tab | Header row | Example data row |
|---|---|---|
| `products` | `sku`, `name`, `category` | `B-17`, `Field Guide`, `Books` |
| `prices` | `sku`, `amount` | `B-17`, `12.50` |

Save the following as `main.go`, set `SPREADSHEET_ID`, and run `go run .`. The example prints `Field Guide: 12.50` for the rows above.

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/viant/gsheets"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := listBooks(ctx, os.Getenv("SPREADSHEET_ID")); err != nil {
		log.Fatal(err)
	}
}

func listBooks(ctx context.Context, id string) error {
	db, err := sql.Open("gsheets", "gsheets://"+id+"?tab=products&tab=prices")
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT p.name, CAST(r.amount AS REAL)
		FROM products AS p JOIN prices AS r ON r.sku = p.sku
		WHERE p.category = ?
		ORDER BY p.name`, "Books")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var amount float64
		if err := rows.Scan(&name, &amount); err != nil {
			return err
		}
		fmt.Printf("%s: %.2f\n", name, amount)
	}
	return rows.Err()
}
```

## Authentication

Enable the Google Sheets API in the credential's Google Cloud project. Share the spreadsheet with the service account or individual user as **Viewer** for reads or **Editor** for writes. The driver selects the matching OAuth scope and defaults to read-only. Read-only mode requests `https://www.googleapis.com/auth/spreadsheets.readonly`. Write mode requests `https://www.googleapis.com/auth/spreadsheets`. A user's previously authorized token must include the selected scope. [Google authentication guide](https://developers.google.com/workspace/sheets/api/scopes)

| DSN option | Meaning |
|---|---|
| `mode=ro` or `mode=rw` | Read-only (default) or opt-in writes. |
| `tab=products&tab=prices` | Expose only these exact grid tab titles. Omit to discover all grid tabs. |
| `credURL`, optional `credKey` | Resolve a mounted Google credentials file or Scy resource, optionally encrypted. URL-encode each query parameter. |
| `credID` | Resolve a registered Scy credential resource. |
| `oauth2ClientURL` and `oauth2TokenURL` | Resolve a Scy OAuth client configuration and refreshable user token, following the Viant BigQuery conventions. |
| No credential options | Use Application Default Credentials, including `GOOGLE_APPLICATION_CREDENTIALS`. |

For instance, `gsheets://SPREADSHEET_ID?mode=rw&credURL=%2Frun%2Fsecrets%2Fgoogle.json`. Applications can use `gsheets.NewConnector(config, options...)` with Google API client options and pass it to `sql.OpenDB`. Each connector owns its credential context and metadata snapshot. Keep separate pools for different identities.

## Tables and query execution

The first row supplies column names. Data starts at sheet row 2. Use SQL identifier quoting for tab and column names containing spaces. The driver names blank header cells `column_N` and suffixes duplicate or reserved column names with `_2`, `_3`, etc. It skips tabs without a header during whole-workbook discovery and reports an error for an explicitly selected tab without a header. Spreadsheet IDs and tab names are separate: worksheet renames affect SQL names. Excel documents in Drive must first be converted to native Google Sheets.

Columns have a stable SQL type of nullable `TEXT`, so a later edit cannot silently change the public schema. Formula cells expose their calculated values. Numbers use unformatted values and dates use Sheets serial numbers. Use `CAST` for numeric predicates, arithmetic and numeric ordering. Missing trailing cells read as SQL NULL. Interior blank cells follow the representation returned by Google. Empty rows are skipped. SQLite's implicit `rowid` is the **physical Sheet row number**, which can change if another editor sorts or inserts rows.

SQLite executes SELECT, prepared positional/named parameters, joins, CTEs, aggregates, sorting and paging locally. The connector batches header discovery and shares its immutable catalog across pooled connections for one minute. `Connector.RefreshSchema()` invalidates it for the next checkout. A data fetch that detects changed headers returns `ErrSchemaChanged` and invalidates the catalog. Each scan fetches the referenced tab as a range; a self-join reuses data within the statement. SQL limits and WHERE predicates generally do not reduce remote bytes. Size memory for the tabs a query touches. There is no cross-query row cache. SQLite schema inspection can run without fetching data rows.

## Writes and atomicity

Open the DSN with `mode=rw`. Use `ExecContext` for INSERT, UPDATE and DELETE. A single successful statement sends at most one write batch. To group statements across tabs, use an explicit transaction:

```go
tx, err := db.BeginTx(ctx, nil)
if err != nil { return err }
defer tx.Rollback()

if _, err := tx.ExecContext(ctx,
    "UPDATE prices SET amount = ? WHERE sku = ?", 12.50, "B-17"); err != nil {
    return err
}
if _, err := tx.ExecContext(ctx,
    "INSERT INTO audit (sku, action) VALUES (?, ?)", "B-17", "repriced"); err != nil {
    return err
}
return tx.Commit()
```

- A transaction snapshots each accessed tab on first use and reads its staged changes. Commit submits **one** `spreadsheets.batchUpdate` for the workbook. Google applies valid requests together or rejects the batch. Rollback discards local changes. The driver never splits a transaction to get around a size limit. [Sheets batch guarantees](https://developers.google.com/workspace/sheets/api/limits)
- INSERT appends using Google's append-cells operation. UPDATE touches only assigned cell values. DELETE clears values across the mapped data columns, preserving formatting and other rows' physical positions. Strings starting with `=` are written as literal strings. Existing formulas outside edited cells remain intact. Row IDs are positional; assigning them, obtaining a durable last-insert ID, and SQL RETURNING are unsupported.
- Commit rechecks the headers and original row values it will change. An observed mismatch returns `ErrSchemaChanged` or `ErrConflict` before submitting the batch. Commits through one connector serialize this check and submission. **External editors can still change a row between those two calls.** Use a coordinated writer when strict concurrency control matters. Explicit isolation levels beyond the driver's default are rejected. Atomicity is limited to one spreadsheet.
- Transport loss or a server timeout after submission can leave the outcome unknown. Use `errors.As(err, &unknown)` with `var unknown *gsheets.CommitUnknownError` and read back the business key before retrying. Inspect Google API failures with `errors.As`; permission failures and canceled reads propagate to callers.

## Datly v1 and SQLX

Blank-import the adapter in both the Datly generator and runtime:

```go
import _ "github.com/viant/gsheets/metadata"
```

The existing SQLX `registry.RegisterDriver` API is sufficient. The adapter loads the driver and associates it with SQLX’s existing SQLite metadata provider. A connector named `book` can use `Driver: gsheets` and a `gsheets://...` DSN. The native Datly transcribe workflow then discovers the tab columns through the virtual tables. Keep the source query and bound filters in DQL/SQL. Backticks work for tab names containing spaces.

## Development

```sh
CGO_ENABLED=0 go test ./...
go test -race ./...
go vet ./...
```

The default tests use synthetic Google API responses and check request counts, bindings, error identity, atomic commit envelopes, rollback and conflict behavior. For optional live write verification, set `GSHEETS_TEST_RW_ID` to a **disposable native spreadsheet** accessible to your ADC identity as Editor. `go test -run '^TestLiveAtomicWrite$' -v` creates two temporary tabs, commits mutations, verifies the values through the driver, and removes those tabs.

Licensed under [Apache-2.0](LICENSE). The SQLite engine is [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3). The virtual-table architecture follows the approach used in [viant/sqlite-vec](https://github.com/viant/sqlite-vec). Another Go driver, [google-sheets-sql](https://github.com/danielloader/google-sheets-sql), pushes queries into Google's visualization and formula engines. Here, SQLite can evaluate joins with Viewer access and no scratch worksheets.
