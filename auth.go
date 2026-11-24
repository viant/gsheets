package gsheets

import (
	"context"
	"errors"
	"fmt"

	"github.com/viant/scy"
	"github.com/viant/scy/cred"
	_ "github.com/viant/scy/kms/blowfish"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// Explicit credentials and options belong to this connector, never the process.
func (c Config) service(ctx context.Context, opts []option.ClientOption) (*sheets.Service, error) {
	scope := sheets.SpreadsheetsReadonlyScope
	if c.ReadWrite {
		scope = sheets.SpreadsheetsScope
	}
	options := []option.ClientOption{option.WithScopes(scope)}
	secrets := scy.New()
	switch {
	case c.CredURL != "" || c.CredID != "":
		resource := &scy.Resource{URL: c.CredURL, Key: c.CredKey}
		// Keep Google-specific fields (such as authorized_user refresh_token)
		// intact when the supplied JSON does not require Scy field decryption.
		if c.CredKey == "" {
			resource.Name = "google_credentials"
		}
		if c.CredID != "" {
			registered := scy.Resources().Lookup(c.CredID)
			if registered == nil {
				return nil, errors.New("gsheets: credential resource not registered")
			}
			local := *registered
			resource = &local
		}
		secret, err := secrets.Load(ctx, resource)
		if err != nil {
			return nil, fmt.Errorf("load Google credential: %w", err)
		}
		options = append(options, option.WithCredentialsJSON([]byte(secret.String())))
	case c.OAuth2ClientURL != "":
		client := &cred.Oauth2Config{}
		secret, err := secrets.Load(ctx, scy.NewResource(client, c.OAuth2ClientURL, "blowfish://default"))
		if err != nil {
			return nil, fmt.Errorf("load OAuth client: %w", err)
		}
		client, ok := secret.Target.(*cred.Oauth2Config)
		if !ok {
			return nil, errors.New("gsheets: invalid OAuth client configuration")
		}
		t, err := secrets.Load(ctx, scy.NewResource(&oauth2.Token{}, c.OAuth2TokenURL, ""))
		if err != nil {
			return nil, fmt.Errorf("load OAuth token: %w", err)
		}
		var token oauth2.Token
		if err = t.Decode(&token); err != nil {
			return nil, fmt.Errorf("decode OAuth token: %w", err)
		}
		// Refresh may occur long after the original Connect call has finished.
		options = append(options, option.WithTokenSource(client.TokenSource(context.WithoutCancel(ctx), &token)))
	}
	options = append(options, opts...)
	return sheets.NewService(ctx, options...)
}
