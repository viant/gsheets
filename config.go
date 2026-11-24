// Package gsheets exposes spreadsheet tabs as database/sql tables.
package gsheets

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var spreadsheetID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Config selects one spreadsheet and one Google identity. An empty Tabs slice
// includes all grid tabs. For large workbooks, scope it to the required tabs.
type Config struct {
	SpreadsheetID                   string
	ReadWrite                       bool
	Tabs                            []string
	CredURL, CredKey, CredID        string
	OAuth2ClientURL, OAuth2TokenURL string
}

// ParseDSN parses gsheets://SPREADSHEET_ID?tab=reports&tab=bundles.
func ParseDSN(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "gsheets" || !spreadsheetID.MatchString(u.Host) || u.User != nil || u.Path != "" && u.Path != "/" || u.Fragment != "" {
		return Config{}, errors.New("gsheets: expected gsheets://SPREADSHEET_ID with optional query parameters")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Config{}, errors.New("gsheets: invalid DSN options")
	}
	c := Config{
		SpreadsheetID: u.Host, Tabs: q["tab"],
		CredURL: q.Get("credURL"), CredKey: q.Get("credKey"), CredID: q.Get("credID"),
		OAuth2ClientURL: q.Get("oauth2ClientURL"), OAuth2TokenURL: q.Get("oauth2TokenURL"),
	}
	switch q.Get("mode") {
	case "", "ro":
	case "rw":
		c.ReadWrite = true
	default:
		return Config{}, errors.New("gsheets: mode must be ro or rw")
	}
	for k, v := range q {
		switch k {
		case "mode", "tab", "credURL", "credKey", "credID", "oauth2ClientURL", "oauth2TokenURL":
		default:
			return Config{}, errors.New("gsheets: unknown DSN option")
		}
		if len(v) > 1 && k != "tab" {
			return Config{}, errors.New("gsheets: repeated DSN option")
		}
	}
	return c, c.validate()
}
func (c Config) validate() error {
	if !spreadsheetID.MatchString(c.SpreadsheetID) {
		return errors.New("gsheets: invalid spreadsheet ID")
	}
	if (c.OAuth2ClientURL == "") != (c.OAuth2TokenURL == "") {
		return errors.New("gsheets: specify both oauth2ClientURL and oauth2TokenURL")
	}
	if c.CredURL != "" && c.CredID != "" || c.CredKey != "" && c.CredURL == "" || (c.CredURL != "" || c.CredID != "") && c.OAuth2ClientURL != "" {
		return errors.New("gsheets: choose one credential source")
	}
	names := map[string]bool{}
	for _, tab := range c.Tabs {
		key := strings.ToLower(tab)
		if tab == "" || names[key] {
			return errors.New("gsheets: empty or duplicate tab selection")
		}
		names[key] = true
	}
	return nil
}
