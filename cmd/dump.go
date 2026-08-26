package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ags-slc/m8/internal/dump"
	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"
)

var (
	dumpSchemas          []string
	dumpStdout           bool
	dumpAllowUnsupported bool
)

var dumpCmd = &cobra.Command{
	Use:   "dump",
	Short: "Export database objects to migration files",
	Long: `Introspects the live database and generates files in the m8 folder layout:
  schema/{pg_schema}/  — CREATE TABLE for each table
  logic/               — CREATE OR REPLACE FUNCTION/PROCEDURE/VIEW
  permissions/         — GRANT statements

Use this to bootstrap m8 on an existing database.

Examples:
  m8 dump --database mydb --user postgres
  m8 dump --database mydb --user postgres --schema public --schema materialized
  m8 dump --database mydb --user postgres --stdout`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		st, err := resolveSettings()
		if err != nil {
			return err
		}

		conn, err := pgx.Connect(ctx, st.ConnStr)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		d := dump.NewDumper(conn)
		d.AllowUnsupported = dumpAllowUnsupported

		schemas := dumpSchemas
		if len(schemas) == 0 {
			var err error
			schemas, err = d.ListSchemas(ctx)
			if err != nil {
				return fmt.Errorf("failed to list schemas: %w", err)
			}
		}

		var totalTables, totalLogic, totalPerms int

		// Logic objects are collected across every schema and named in one
		// pass at the end: overloaded functions share a name, so filenames can
		// only be made collision-free once the whole set is known.
		type logicEntry struct {
			object   dump.LogicObject
			rendered string
		}
		var logicEntries []logicEntry

		for _, schema := range schemas {
			// A schema name becomes a directory component below. Rejecting it
			// here rather than in writeFile is the point: "../ops" cleans away
			// to "ops", which writeFile would accept as perfectly local.
			if !dumpStdout && !safeComponent(schema) {
				return fmt.Errorf("refusing to dump schema %q: name is not usable as a path component; rename it or use --stdout", schema)
			}

			// --- Tables → schema/{pg_schema}/*.sql ---
			tables, err := d.ListTables(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list tables in %s: %w", schema, err)
			}

			for _, tableName := range tables {
				table, err := d.DumpTable(ctx, schema, tableName)
				if err != nil {
					return fmt.Errorf("failed to dump %s.%s: %w", schema, tableName, err)
				}

				ddl := dump.RenderDDL(table)

				if dumpStdout {
					fmt.Printf("-- schema/%s/%s.sql\n%s\n", schema, tableName, ddl)
				} else {
					// No dedup machinery here as there is for logic names, so
					// sanitizing would silently merge "a/b" into "a_b" and lose one
					// of them. dump is interactive; fail closed instead.
					if !safeComponent(tableName) {
						return fmt.Errorf("refusing to dump %s.%s: table name is not usable as a filename; rename it or use --stdout", schema, tableName)
					}
					if err := writeFile(st.MigrationsDir, filepath.Join("schema", schema), tableName+".sql", ddl); err != nil {
						return err
					}
				}
				totalTables++
			}

			// --- Functions/Procedures → logic/*.sql ---
			funcs, err := d.ListFunctions(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list functions in %s: %w", schema, err)
			}

			for _, f := range funcs {
				logicEntries = append(logicEntries, logicEntry{
					object:   dump.LogicObject{Schema: schema, Name: f.Name, Identity: f.Identity},
					rendered: dump.RenderFunction(&f),
				})
			}

			// --- Views → logic/*.sql ---
			views, err := d.ListViews(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list views in %s: %w", schema, err)
			}

			for _, v := range views {
				logicEntries = append(logicEntries, logicEntry{
					object:   dump.LogicObject{Schema: schema, Name: v.Name},
					rendered: dump.RenderView(&v),
				})
			}

			// --- Grants → permissions/grants_{schema}.sql ---
			//
			// Order matters: USAGE on the schema, then the revokes that undo
			// PostgreSQL's PUBLIC defaults, then the grants. Without the schema
			// grant every relation grant below it is inert on a rebuild.
			schemaGrants, err := d.ListSchemaGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list schema grants for %s: %w", schema, err)
			}
			grants, err := d.ListGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list grants in %s: %w", schema, err)
			}
			columnGrants, err := d.ListColumnGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list column grants in %s: %w", schema, err)
			}
			routineGrants, err := d.ListRoutineGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list routine grants in %s: %w", schema, err)
			}
			revokes, err := d.ListPublicRevokes(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list public revokes in %s: %w", schema, err)
			}

			allGrants := append(grants, columnGrants...)
			if len(allGrants) > 0 || len(routineGrants) > 0 || len(schemaGrants) > 0 || len(revokes) > 0 {
				var sections []string
				if r := dump.RenderSchemaGrants(schemaGrants); r != "" {
					sections = append(sections, r)
				}
				if r := dump.RenderPublicRevokes(revokes); r != "" {
					sections = append(sections, r)
				}
				if len(allGrants) > 0 {
					sections = append(sections, dump.RenderGrants(allGrants, schema))
				} else {
					sections = append(sections, fmt.Sprintf("-- Grants for schema %s\n", schema))
				}
				if r := dump.RenderRoutineGrants(routineGrants); r != "" {
					sections = append(sections, r)
				}
				rendered := strings.Join(sections, "\n")
				filename := "grants_" + schema + ".sql"

				if dumpStdout {
					fmt.Printf("-- permissions/%s\n%s\n", filename, rendered)
				} else {
					if err := writeFile(st.MigrationsDir, "permissions", filename, rendered); err != nil {
						return err
					}
				}
				totalPerms++
			}
		}

		objects := make([]dump.LogicObject, 0, len(logicEntries))
		for _, e := range logicEntries {
			objects = append(objects, e.object)
		}
		logicNames := dump.ResolveLogicFileNames(objects)
		for _, e := range logicEntries {
			filename := logicNames[e.object]
			if dumpStdout {
				fmt.Printf("-- logic/%s\n%s\n", filename, e.rendered)
			} else {
				if err := writeFile(st.MigrationsDir, "logic", filename, e.rendered); err != nil {
					return err
				}
			}
			totalLogic++
		}

		if !dumpStdout {
			parts := []string{}
			if totalTables > 0 {
				parts = append(parts, fmt.Sprintf("%d tables", totalTables))
			}
			if totalLogic > 0 {
				parts = append(parts, fmt.Sprintf("%d logic objects", totalLogic))
			}
			if totalPerms > 0 {
				parts = append(parts, fmt.Sprintf("%d permission files", totalPerms))
			}
			fmt.Printf("\nDumped %s across %d schemas.\n", strings.Join(parts, ", "), len(schemas))
		}

		return nil
	},
}

// safeComponent reports whether s is usable as exactly one path element.
//
// Catalog names reach the filesystem as path components, and PostgreSQL lets a
// quoted identifier hold "/" and ".." -- so this is what stops a hostile object
// name from steering a write. filepath.IsLocal additionally rejects Windows
// drive-relative paths and reserved device names.
func safeComponent(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.ContainsAny(s, `/\`) &&
		!strings.ContainsRune(s, 0) &&
		filepath.IsLocal(s)
}

// openRoot creates root if it is missing and opens it for confined access.
func openRoot(root string) (*os.Root, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory %s: %w", root, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", root, err)
	}
	return r, nil
}

// writeFile writes content to <root>/<relDir>/<filename>.
//
// relDir and filename are built from catalog names -- schema, table, function
// and view names -- which anyone holding CREATE on a schema controls and which
// may legally contain "/" and "..". os.Root confines every mkdir and open
// beneath root, so a hostile identifier can neither climb out of the migrations
// tree nor follow a symlink out of it.
//
// The caller still has to reject an unsafe name BEFORE joining it into relDir:
// filepath.Join("schema", "../ops") cleans to "ops", which is local, exists, and
// whose contents apply runs verbatim.
func writeFile(root, relDir, filename, content string) error {
	if !filepath.IsLocal(relDir) {
		return fmt.Errorf("refusing to write: directory %q escapes the migrations directory", relDir)
	}
	if !safeComponent(filename) {
		return fmt.Errorf("refusing to write: %q is not a plain filename", filename)
	}
	r, err := openRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	if err := r.MkdirAll(relDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", filepath.Join(root, relDir), err)
	}
	rel := filepath.Join(relDir, filename)
	if err := r.WriteFile(rel, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", filepath.Join(root, rel), err)
	}
	fmt.Printf("  %s\n", filepath.Join(root, rel))
	return nil
}

func init() {
	dumpCmd.Flags().StringSliceVar(&dumpSchemas, "schema", nil, "Schemas to dump (default: all user schemas)")
	dumpCmd.Flags().BoolVar(&dumpStdout, "stdout", false, "Print DDL to stdout instead of writing files")
	dumpCmd.Flags().BoolVar(&dumpAllowUnsupported, "allow-unsupported", false,
		"Leave objects m8 cannot represent (materialized views) out of the dump instead of refusing")
	rootCmd.AddCommand(dumpCmd)
}
