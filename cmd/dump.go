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
	dumpSchemas []string
	dumpStdout  bool
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
		connStr := resolveConnStr()

		conn, err := pgx.Connect(ctx, connStr)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		d := dump.NewDumper(conn)

		schemas := dumpSchemas
		if len(schemas) == 0 {
			var err error
			schemas, err = d.ListSchemas(ctx)
			if err != nil {
				return fmt.Errorf("failed to list schemas: %w", err)
			}
		}

		var totalTables, totalLogic, totalPerms int

		for _, schema := range schemas {
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
					if err := writeFile(filepath.Join(flagMigrationsDir, "schema", schema), tableName+".sql", ddl); err != nil {
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
				rendered := dump.RenderFunction(&f)
				filename := f.Name + ".sql"
				// Prefix with schema if not public to avoid collisions
				if schema != "public" {
					filename = schema + "_" + f.Name + ".sql"
				}

				if dumpStdout {
					fmt.Printf("-- logic/%s\n%s\n", filename, rendered)
				} else {
					if err := writeFile(filepath.Join(flagMigrationsDir, "logic"), filename, rendered); err != nil {
						return err
					}
				}
				totalLogic++
			}

			// --- Views → logic/*.sql ---
			views, err := d.ListViews(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list views in %s: %w", schema, err)
			}

			for _, v := range views {
				rendered := dump.RenderView(&v)
				filename := v.Name + ".sql"
				if schema != "public" {
					filename = schema + "_" + v.Name + ".sql"
				}

				if dumpStdout {
					fmt.Printf("-- logic/%s\n%s\n", filename, rendered)
				} else {
					if err := writeFile(filepath.Join(flagMigrationsDir, "logic"), filename, rendered); err != nil {
						return err
					}
				}
				totalLogic++
			}

			// --- Grants → permissions/grants_{schema}.sql ---
			grants, err := d.ListGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list grants in %s: %w", schema, err)
			}
			publicGrants, err := d.ListPublicGrants(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list public grants in %s: %w", schema, err)
			}

			allGrants := append(grants, publicGrants...)
			if len(allGrants) > 0 {
				rendered := dump.RenderGrants(allGrants, schema)
				filename := "grants_" + schema + ".sql"

				if dumpStdout {
					fmt.Printf("-- permissions/%s\n%s\n", filename, rendered)
				} else {
					if err := writeFile(filepath.Join(flagMigrationsDir, "permissions"), filename, rendered); err != nil {
						return err
					}
				}
				totalPerms++
			}
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

func writeFile(dir, filename, content string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", filePath, err)
	}
	fmt.Printf("  %s\n", filePath)
	return nil
}

func init() {
	dumpCmd.Flags().StringSliceVar(&dumpSchemas, "schema", nil, "Schemas to dump (default: all user schemas)")
	dumpCmd.Flags().BoolVar(&dumpStdout, "stdout", false, "Print DDL to stdout instead of writing files")
	rootCmd.AddCommand(dumpCmd)
}
