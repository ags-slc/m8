package cmd

import (
	"fmt"
	"os"
	"path/filepath"

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
	Short: "Export database schema to migration files",
	Long: `Introspects the live database and generates schema/ files with CREATE TABLE
statements for each table. Use this to bootstrap m8 on an existing database.

By default, exports all user schemas. Use --schema to limit to specific schemas.

Examples:
  m8 dump --database mydb --user postgres
  m8 dump --database mydb --user postgres --schema public --schema materialized
  m8 dump --database mydb --user postgres --stdout  # print to stdout instead of files`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		connStr := resolveConnStr()

		conn, err := pgx.Connect(ctx, connStr)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		defer conn.Close(ctx)

		d := dump.NewDumper(conn)

		// Determine which schemas to dump
		schemas := dumpSchemas
		if len(schemas) == 0 {
			var err error
			schemas, err = d.ListSchemas(ctx)
			if err != nil {
				return fmt.Errorf("failed to list schemas: %w", err)
			}
		}

		totalTables := 0

		for _, schema := range schemas {
			tables, err := d.ListTables(ctx, schema)
			if err != nil {
				return fmt.Errorf("failed to list tables in %s: %w", schema, err)
			}

			if len(tables) == 0 {
				continue
			}

			for _, tableName := range tables {
				table, err := d.DumpTable(ctx, schema, tableName)
				if err != nil {
					return fmt.Errorf("failed to dump %s.%s: %w", schema, tableName, err)
				}

				ddl := dump.RenderDDL(table)

				if dumpStdout {
					fmt.Printf("-- %s.%s\n%s\n", schema, tableName, ddl)
				} else {
					dir := filepath.Join(flagMigrationsDir, "schema", schema)
					if err := os.MkdirAll(dir, 0755); err != nil {
						return fmt.Errorf("failed to create directory %s: %w", dir, err)
					}
					filePath := filepath.Join(dir, tableName+".sql")
					if err := os.WriteFile(filePath, []byte(ddl), 0644); err != nil {
						return fmt.Errorf("failed to write %s: %w", filePath, err)
					}
					fmt.Printf("  %s\n", filePath)
				}

				totalTables++
			}
		}

		if !dumpStdout {
			fmt.Printf("\nDumped %d tables across %d schemas.\n", totalTables, len(schemas))
		}

		return nil
	},
}

func init() {
	dumpCmd.Flags().StringSliceVar(&dumpSchemas, "schema", nil, "Schemas to dump (default: all user schemas)")
	dumpCmd.Flags().BoolVar(&dumpStdout, "stdout", false, "Print DDL to stdout instead of writing files")
	rootCmd.AddCommand(dumpCmd)
}
