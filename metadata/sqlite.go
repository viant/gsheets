// Package metadata registers gsheets with SQLX for SQLite-compatible schema
// discovery. Datly hosts can blank-import this package alongside the driver.
package metadata

import (
	"reflect"

	"github.com/viant/gsheets"
	"github.com/viant/sqlx/metadata/product/sqlite"
	"github.com/viant/sqlx/metadata/registry"
)

func init() {
	t := reflect.TypeFor[gsheets.Driver]()
	if err := registry.RegisterDriver(*sqlite.SQLite3(), t.PkgPath(), t.Name()); err != nil {
		panic(err)
	}
}
