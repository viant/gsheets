package gsheets

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/api/sheets/v4"
)

// ErrSchemaChanged means the header no longer matches the prepared schema.
// The affected physical connection is discarded when returned to the pool.
var ErrSchemaChanged = errors.New("gsheets: worksheet schema changed")

type sheet struct {
	id              int64
	title           string
	header, columns []string
}

func byID(id int64, headerOnly bool) *sheets.DataFilter {
	r := &sheets.GridRange{SheetId: id, ForceSendFields: []string{"SheetId"}}
	if headerOnly {
		r.EndRowIndex = 1
	}
	return &sheets.DataFilter{GridRange: r}
}
func fetch(ctx context.Context, api *sheets.Service, cfg Config, tabs []sheet, headerOnly bool) (map[int64][][]any, error) {
	// Google's batchGetByDataFilter endpoint requires a write-capable scope.
	// Read-only credentials use the standard A1 batchGet endpoint.
	if !cfg.ReadWrite {
		out := make(map[int64][][]any, len(tabs))
		for chunk := range slices.Chunk(tabs, 32) {
			ranges := make([]string, len(chunk))
			for i, t := range chunk {
				ranges[i] = "'" + strings.ReplaceAll(t.title, "'", "''") + "'"
				if headerOnly {
					ranges[i] += "!1:1"
				}
			}
			resp, err := api.Spreadsheets.Values.BatchGet(cfg.SpreadsheetID).Ranges(ranges...).ValueRenderOption("UNFORMATTED_VALUE").DateTimeRenderOption("SERIAL_NUMBER").Context(ctx).Do()
			if err != nil {
				return nil, fmt.Errorf("read worksheet values: %w", err)
			}
			if len(resp.ValueRanges) != len(chunk) {
				return nil, errors.New("gsheets: incomplete range response")
			}
			for i, v := range resp.ValueRanges {
				out[chunk[i].id] = v.Values
			}
		}
		return out, nil
	}
	filters := make([]*sheets.DataFilter, len(tabs))
	for i, t := range tabs {
		filters[i] = byID(t.id, headerOnly)
	}
	req := &sheets.BatchGetValuesByDataFilterRequest{DataFilters: filters, ValueRenderOption: "UNFORMATTED_VALUE", DateTimeRenderOption: "SERIAL_NUMBER"}
	resp, err := api.Spreadsheets.Values.BatchGetByDataFilter(cfg.SpreadsheetID, req).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("read worksheet values: %w", err)
	}
	out := make(map[int64][][]any, len(filters))
	for _, r := range resp.ValueRanges {
		if r.ValueRange == nil {
			continue
		}
		for _, f := range r.DataFilters {
			if f.GridRange != nil {
				out[f.GridRange.SheetId] = r.ValueRange.Values
			}
		}
	}
	for _, f := range filters {
		if _, ok := out[f.GridRange.SheetId]; !ok {
			return nil, errors.New("gsheets: missing requested worksheet in API response")
		}
	}
	return out, nil
}

func catalog(ctx context.Context, api *sheets.Service, c Config) ([]sheet, error) {
	book, err := api.Spreadsheets.Get(c.SpreadsheetID).Fields("sheets(properties(sheetId,title,sheetType))").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("discover spreadsheet: %w", err)
	}
	selected := map[string]bool{}
	for _, t := range c.Tabs {
		selected[t] = false
	}
	var tabs []sheet

	for _, t := range book.Sheets {
		p := t.Properties
		if p == nil || p.SheetType != "GRID" {
			continue
		}
		if len(selected) > 0 {
			if _, ok := selected[p.Title]; !ok {
				continue
			}
			selected[p.Title] = true
		}
		tabs = append(tabs, sheet{id: p.SheetId, title: p.Title})

	}
	for name, found := range selected {
		if !found {
			return nil, fmt.Errorf("gsheets: grid tab %q not found", name)
		}
	}
	if len(tabs) == 0 {
		return nil, nil
	}
	headers, err := fetch(ctx, api, c, tabs, true)
	if err != nil {
		return nil, err
	}
	result := tabs[:0]
	for _, t := range tabs {
		t.header = firstRow(headers[t.id])
		if len(t.header) == 0 {
			if selected[t.title] {
				return nil, fmt.Errorf("gsheets: tab %q has no header", t.title)
			}
			continue
		}
		used := map[string]bool{"rowid": true, "_rowid_": true, "oid": true}
		for i, name := range t.header {
			if name == "" {
				name = "column_" + strconv.Itoa(i+1)
			}
			base := name
			for j := 2; used[strings.ToLower(name)]; j++ {
				name = base + "_" + strconv.Itoa(j)
			}
			used[strings.ToLower(name)] = true
			t.columns = append(t.columns, name)
		}
		result = append(result, t)
	}
	return result, nil
}

func firstRow(rows [][]any) []string {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, len(rows[0]))
	for i, v := range rows[0] {
		if v != nil {
			names[i] = fmt.Sprint(v)
		}
	}
	return names
}

func (c *conn) load(t *sheet) ([][]any, error) {
	s := c.active
	if rows, ok := s.data[t.id]; ok {
		return rows, nil
	}
	values, err := fetch(c.db.GetInterrupt(), c.api, c.cfg, []sheet{*t}, false)
	if err != nil {
		return nil, err
	}
	rows := values[t.id]
	if !slices.Equal(firstRow(rows), t.header) {
		c.invalidate()
		return nil, fmt.Errorf("%w: %s", ErrSchemaChanged, t.title)
	}
	for _, row := range rows[1:] {
		if len(row) > len(t.header) {
			return nil, fmt.Errorf("gsheets: tab %q contains data beyond the header", t.title)
		}
	}
	s.data[t.id] = rows[1:]
	return s.data[t.id], nil
}
