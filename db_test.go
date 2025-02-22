package runn

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/k1LoW/runn/testutil"
)

func TestDBRun(t *testing.T) {
	tests := []struct {
		stmt string
		want map[string]any
	}{
		{
			"SELECT 1",
			map[string]any{
				"rows": []map[string]any{
					{"1": int64(1)},
				},
				"run": true,
			},
		},
		{
			"SELECT 1;SELECT 2;",
			map[string]any{
				"rows": []map[string]any{
					{"2": int64(2)},
				},
				"run": true,
			},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );
INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));`,
			map[string]any{
				"last_insert_id": int64(1),
				"rows_affected":  int64(1),
				"run":            true,
			},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );
INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));
SELECT COUNT(*) AS count FROM users;
`,
			map[string]any{
				"rows": []map[string]any{
					{"count": int64(1)},
				},
				"run": true,
			},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC,
		  info JSON
        );
INSERT INTO users (username, password, email, created, info) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'), '{
	"age": 20,
	"address": {
		"city": "Tokyo",
		"country": "Japan"
	}
}');
SELECT * FROM users;
`,
			map[string]any{
				"rows": []map[string]any{
					{
						"id":       int64(1),
						"username": "alice",
						"password": "passw0rd",
						"email":    "alice@example.com",
						"created":  "2017-12-05 00:00:00",
						"updated":  nil,
						"info": map[string]any{
							"age": float64(20),
							"address": map[string]any{
								"city":    "Tokyo",
								"country": "Japan",
							},
						},
					},
				},
				"run": true,
			},
		},
	}
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.stmt, func(t *testing.T) {
			_, dsn := testutil.SQLite(t)
			o, err := New()
			if err != nil {
				t.Fatal(err)
			}
			r, err := newDBRunner("db", dsn)
			if err != nil {
				t.Fatal(err)
			}
			r.operator = o
			q := &dbQuery{stmt: tt.stmt}
			if err := r.Run(ctx, q); err != nil {
				t.Error(err)
				return
			}
			got := o.store.steps[0]
			if diff := cmp.Diff(got, tt.want, nil); diff != "" {
				t.Error(diff)
			}
		})

		t.Run(fmt.Sprintf("%s with Tx", tt.stmt), func(t *testing.T) {
			db, dsn := testutil.SQLite(t)
			o, err := New()
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			})
			r, err := newDBRunner("db", dsn)
			if err != nil {
				t.Fatal(err)
			}
			nt, err := nestTx(tx)
			if err != nil {
				t.Fatal(err)
			}
			r.client = nt
			r.operator = o
			q := &dbQuery{stmt: tt.stmt}
			if err := r.Run(ctx, q); err != nil {
				t.Error(err)
				return
			}
			got := o.store.steps[0]
			if diff := cmp.Diff(got, tt.want, nil); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSeparateStmt(t *testing.T) {
	tests := []struct {
		stmt string
		want []string
	}{
		{
			"SELECT 1",
			[]string{"SELECT 1"},
		},
		{
			"SELECT 1;SELECT 2;",
			[]string{"SELECT 1;", "SELECT 2;"},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );
INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));`,
			[]string{
				`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );`,
				"INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));",
			},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );
INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));
SELECT COUNT(*) AS count FROM users;
`,
			[]string{
				`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC
        );`,
				"INSERT INTO users (username, password, email, created) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'));",
				"SELECT COUNT(*) AS count FROM users;",
			},
		},
		{
			`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC,
		  info JSON
        );
INSERT INTO users (username, password, email, created, info) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'), '{
	"age": 20,
	"address": {
		"city": "Tokyo",
		"country": "Japan"
	}
}');
SELECT * FROM users;
`,
			[]string{
				`CREATE TABLE users (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          username TEXT UNIQUE NOT NULL,
          password TEXT NOT NULL,
          email TEXT UNIQUE NOT NULL,
          created NUMERIC NOT NULL,
          updated NUMERIC,
		  info JSON
        );`,
				`INSERT INTO users (username, password, email, created, info) VALUES ('alice', 'passw0rd', 'alice@example.com', datetime('2017-12-05'), '{
	"age": 20,
	"address": {
		"city": "Tokyo",
		"country": "Japan"
	}
}');`,
				"SELECT * FROM users;",
			},
		},
	}
	for _, tt := range tests {
		got := separateStmt(tt.stmt)
		if diff := cmp.Diff(got, tt.want, nil); diff != "" {
			t.Error(diff)
		}
	}
}
