package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIdentityArrayLocaleProbe(t *testing.T) {
	fixture := newAuthFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect isolated locale probe: %T", err)
	}
	defer connection.Close(ctx)
	var underC, underEnglish bool
	err = connection.QueryRow(ctx, `
		SELECT pm.text_array_is_strictly_sorted(ARRAY['Z', 'a'] COLLATE "C"),
		       pm.text_array_is_strictly_sorted(ARRAY['Z', 'a'] COLLATE "en-US-x-icu")
	`).Scan(&underC, &underEnglish)
	if err != nil {
		t.Fatalf("execute isolated locale probe: %T", err)
	}
	t.Logf("canonical byte-order probe: C=%t English=%t", underC, underEnglish)
	if !underC || !underEnglish {
		t.Fatal("canonical identifier ordering depends on caller collation")
	}
}
