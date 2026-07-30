// dbcheck is a tiny Postgres query helper used by scripts/test.sh's
// --full-deploy suite to assert state-store rows after each lifecycle
// step. It is intentionally minimal: take a query, run it, print one
// row per line as tab-separated columns. The exit code reflects DB
// errors only — empty result sets are NOT failures (the bash caller
// decides what to assert).
//
// Build:
//
//	cd apps/orchestrator && go build -o /tmp/dbcheck ./dbcheck
//
// Use (DATABASE_URL must be set):
//
//	/tmp/dbcheck "SELECT vm_id, state FROM orchestrator_vms WHERE vm_id=$1" "fuse-foo"
//	# → "fuse-foo	running"
//
// The helper prints column names as the first line when --headers is
// passed (handy for ad-hoc debugging), otherwise just data rows. NULL
// columns render as the literal string "NULL" so the bash caller can
// distinguish them from empty strings.
//
// Why a dedicated binary instead of psql: we don't want to require
// every developer / CI runner to have psql installed. We already
// depend on jackc/pgx (used by apps/orchestrator/state_store_postgres.go),
// so reusing it keeps the toolchain to "just go".
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// version is stamped at release time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	headers := flag.Bool("headers", false, "print column headers as first line")
	timeout := flag.Duration("timeout", 15*time.Second, "query timeout")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dbcheck [--headers] [--timeout=15s] <query> [args...]")
		fmt.Fprintln(os.Stderr, "  reads DATABASE_URL from env; prints one row per line as TSV")
	}
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "dbcheck: DATABASE_URL not set")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbcheck: open: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	args := make([]any, flag.NArg()-1)
	for i, a := range flag.Args()[1:] {
		args[i] = a
	}

	rows, err := db.QueryContext(ctx, flag.Arg(0), args...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbcheck: query: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbcheck: columns: %v\n", err)
		os.Exit(1)
	}

	if *headers {
		fmt.Println(strings.Join(cols, "\t"))
	}

	values := make([]any, len(cols))
	scan := make([]any, len(cols))
	for i := range values {
		scan[i] = &values[i]
	}

	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			fmt.Fprintf(os.Stderr, "dbcheck: scan: %v\n", err)
			os.Exit(1)
		}
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = formatValue(v)
		}
		fmt.Println(strings.Join(parts, "\t"))
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "dbcheck: rows: %v\n", err)
		os.Exit(1)
	}
}

// formatValue renders a single column. NULL → "NULL"; bytea → hex.
// Tabs and newlines in text fields are replaced with spaces so the
// TSV format stays parseable on the bash side.
func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		// bytea (auth_token_encrypted, etc.) — hex prefix so callers
		// can distinguish "empty bytes" from "empty string".
		return fmt.Sprintf("0x%x", x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case string:
		return strings.NewReplacer("\t", " ", "\n", " ").Replace(x)
	default:
		s := fmt.Sprintf("%v", x)
		return strings.NewReplacer("\t", " ", "\n", " ").Replace(s)
	}
}
