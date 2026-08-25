package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var newCmd = &cobra.Command{
	Use:   "new <type> <name>",
	Short: "Create a new migration file",
	Long: `Scaffolds a new migration file in the correct directory.

Types and naming:
  m8 new schema public/users       → migrations/schema/public/users.sql
  m8 new schema materialized/rpt   → migrations/schema/materialized/rpt.sql
  m8 new logic proc_refresh        → migrations/logic/proc_refresh.sql
  m8 new permissions grants        → migrations/permissions/grants.sql
  m8 new ops "create extensions"   → migrations/ops/{timestamp}__create_extensions.sql`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		migType := strings.ToLower(args[0])
		name := args[1]

		// The same resolution apply uses: scaffolding into migrations/ while
		// apply reads .m8.yaml's migrations_dir writes files nothing runs.
		st, err := resolveSettings()
		if err != nil {
			return err
		}
		migrationsDir := st.MigrationsDir

		var filePath string

		switch migType {
		case "schema":
			// name should be like "public/users" or "materialized/rpt_invoice"
			parts := strings.SplitN(name, "/", 2)
			if len(parts) != 2 {
				return fmt.Errorf("schema name must include PG schema: m8 new schema public/users")
			}
			pgSchema, objName := parts[0], parts[1]
			dir := filepath.Join(migrationsDir, "schema", pgSchema)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
			filePath = filepath.Join(dir, objName+".sql")

		case "logic":
			dir := filepath.Join(migrationsDir, "logic")
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
			filePath = filepath.Join(dir, name+".sql")

		case "permissions":
			dir := filepath.Join(migrationsDir, "permissions")
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
			filePath = filepath.Join(dir, name+".sql")

		case "ops":
			dir := filepath.Join(migrationsDir, "ops")
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}
			ts := time.Now().UTC().Format("20060102_150405")
			safeName := strings.ReplaceAll(strings.ToLower(name), " ", "_")
			filePath = filepath.Join(dir, ts+"__"+safeName+".sql")

		default:
			return fmt.Errorf("unknown type %q (expected: schema, logic, permissions, ops)", migType)
		}

		// Check if file already exists
		if _, err := os.Stat(filePath); err == nil {
			return fmt.Errorf("file already exists: %s", filePath)
		}

		// Write template content
		content := templateFor(migType, name)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", filePath, err)
		}

		fmt.Printf("Created %s\n", filePath)
		return nil
	},
}

func templateFor(migType, name string) string {
	switch migType {
	case "schema":
		parts := strings.SplitN(name, "/", 2)
		objName := name
		if len(parts) == 2 {
			objName = parts[1]
		}
		return fmt.Sprintf("CREATE TABLE %s (\n    id BIGSERIAL PRIMARY KEY\n);\n", objName)
	case "logic":
		return fmt.Sprintf("CREATE OR REPLACE FUNCTION %s()\nRETURNS void\nLANGUAGE plpgsql AS $$\nBEGIN\n    -- TODO\nEND;\n$$;\n", name)
	case "permissions":
		return "-- Define grants, revokes, and role permissions here.\n"
	case "ops":
		return "-- One-time operation. This file runs once in order.\n"
	default:
		return ""
	}
}

func init() {
	rootCmd.AddCommand(newCmd)
}
