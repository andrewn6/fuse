// migrategen diffs the ent schema against the migrations directory and
// writes a plain numbered .sql file, matching the ApplyMigrations
// convention used by the orchestrator (lexically ordered files, applied
// once each inside a transaction).
//
// usage: DEV_DATABASE_URL=postgres://... go run ./internal/entspike/migrategen <name>
// the dev database must be empty; ent replays existing migrations onto it
// and diffs the result against the schema graph.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"text/template"

	atlasmigrate "ariga.io/atlas/sql/migrate"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/folsomintel/fuse/internal/entspike/ent/migrate"
)

func init() {
	// ent resolves the driver from the url scheme ("postgres");
	// register pgx under that name instead of pulling in lib/pq
	sql.Register("postgres", stdlib.GetDefaultDriver())
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: migrategen <migration-name, e.g. 0001_ent_baseline>")
	}
	name := os.Args[1]
	devURL := os.Getenv("DEV_DATABASE_URL")
	if devURL == "" {
		log.Fatal("DEV_DATABASE_URL not set")
	}

	dir, err := atlasmigrate.NewLocalDir("internal/entspike/migrations")
	if err != nil {
		log.Fatalf("open migrations dir: %v", err)
	}

	// plain "<name>.sql" files with one statement per line, like the
	// hand-written files in internal/orchestrator/migrations
	fmtr, err := atlasmigrate.NewTemplateFormatter(
		template.Must(template.New("name").Parse("{{ .Name }}.sql")),
		template.Must(template.New("content").Parse(
			"{{ range .Changes }}{{ with .Comment }}-- {{ . }}\n{{ end }}{{ .Cmd }};\n{{ end }}")),
	)
	if err != nil {
		log.Fatalf("formatter: %v", err)
	}

	opts := []schema.MigrateOption{
		schema.WithDir(dir),
		schema.WithMigrationMode(schema.ModeReplay),
		schema.WithDialect(dialect.Postgres),
		schema.WithFormatter(fmtr),
	}
	if err := migrate.NamedDiff(context.Background(), devURL, name, opts...); err != nil {
		log.Fatalf("diff: %v", err)
	}
	fmt.Println("wrote migration", name)
}
