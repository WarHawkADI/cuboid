package cdc

import "testing"

// A store's own database is reached with the main DSN's role and server.
func TestWithDatabase(t *testing.T) {
	cases := []struct{ dsn, db, want string }{
		{"postgres://cuboid_cdc:pw@maindb:5432/shop", "store_42", "postgres://cuboid_cdc:pw@maindb:5432/store_42"},
		{"postgres://cuboid_cdc:pw@maindb:5432/shop?sslmode=disable", "store_7", "postgres://cuboid_cdc:pw@maindb:5432/store_7?sslmode=disable"},
		{"host=maindb user=cuboid_cdc dbname=shop", "store_42", "host=maindb user=cuboid_cdc dbname=store_42"},
		{"host=maindb user=cuboid_cdc", "store_42", "host=maindb user=cuboid_cdc dbname=store_42"},
	}
	for _, c := range cases {
		got, err := WithDatabase(c.dsn, c.db)
		if err != nil || got != c.want {
			t.Errorf("%s -> %s: got %q (%v), want %q", c.dsn, c.db, got, err, c.want)
		}
	}
}
