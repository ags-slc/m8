package migration

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Type represents the kind of migration.
type Type int

const (
	// TypeOps migrations run once in timestamp order (extensions, hypertables, one-time operations).
	TypeOps Type = iota
	// TypeSchema migrations are declarative desired-state files, auto-diffed per PG schema.
	TypeSchema
	// TypeLogic migrations re-run when their content changes (procedures, functions, views, cron).
	TypeLogic
	// TypePermissions migrations re-run when their content changes (grants, revokes, roles).
	TypePermissions
)

// String returns the type name for display and database storage.
func (t Type) String() string {
	switch t {
	case TypeOps:
		return "ops"
	case TypeSchema:
		return "schema"
	case TypeLogic:
		return "logic"
	case TypePermissions:
		return "permissions"
	default:
		return "unknown"
	}
}

// Folder names for the directory-based layout.
const (
	FolderOps         = "ops"
	FolderSchema      = "schema"
	FolderLogic       = "logic"
	FolderPermissions = "permissions"
)

// Migration represents a single migration file.
type Migration struct {
	Type     Type
	Version  string // Timestamp prefix for ops; empty for others.
	Name     string // Human-readable name derived from filename.
	PGSchema string // Target PostgreSQL schema (from schema/ subfolder name); empty for non-schema types.
	Filename string // Relative path from migrations root (e.g., "schema/public/users.sql").
	FilePath string // Absolute path on disk.
	Checksum string // SHA-256 hex digest of file content.
	Content  []byte // Raw file content.
}

// Regex for ops/ files: {timestamp}__{name}.sql or {timestamp}_{seq}__{name}.sql
var opsPattern = regexp.MustCompile(`^(\d+(?:_\d+)*)__(.+)\.sql$`)

// Regex for simple .sql files in logic/, permissions/, schema/ subfolders.
var simplePattern = regexp.MustCompile(`^(.+)\.sql$`)

// Legacy prefix patterns for backward compatibility with flat layout.
var (
	legacyVersionedPattern  = regexp.MustCompile(`^V(\d+(?:_\d+)*)__(.+)\.sql$`)
	legacyRepeatablePattern = regexp.MustCompile(`^R__(.+)\.sql$`)
	legacySchemaPattern     = regexp.MustCompile(`^S__(.+)\.sql$`)
)

// Discover scans a migrations directory for migration files and returns them
// sorted: ops by version, then schema alphabetically, then logic, then permissions.
//
// Supports two layouts:
//   - Folder-based (preferred): ops/, schema/{pg_schema}/, logic/, permissions/
//   - Flat (legacy): V__, S__, R__ prefixed files in a single directory
func Discover(dir string) ([]*Migration, error) {
	var migrations []*Migration

	folderBased := false
	for _, sub := range []string{FolderOps, FolderSchema, FolderLogic, FolderPermissions} {
		subDir := filepath.Join(dir, sub)
		if info, err := os.Stat(subDir); err == nil && info.IsDir() {
			folderBased = true
			break
		}
	}

	if folderBased {
		m, err := discoverFolderLayout(dir)
		if err != nil {
			return nil, err
		}
		migrations = m
	} else {
		m, err := discoverFlatLayout(dir)
		if err != nil {
			return nil, err
		}
		migrations = m
	}

	sort.Slice(migrations, func(i, j int) bool {
		if migrations[i].Type != migrations[j].Type {
			return migrations[i].Type < migrations[j].Type
		}
		if migrations[i].Type == TypeOps {
			return migrations[i].Version < migrations[j].Version
		}
		// For schema, sort by PGSchema then Name
		if migrations[i].Type == TypeSchema {
			if migrations[i].PGSchema != migrations[j].PGSchema {
				return migrations[i].PGSchema < migrations[j].PGSchema
			}
		}
		return migrations[i].Name < migrations[j].Name
	})

	return migrations, nil
}

func discoverFolderLayout(dir string) ([]*Migration, error) {
	var migrations []*Migration

	// ops/
	if m, err := scanSimpleFolder(filepath.Join(dir, FolderOps), FolderOps, TypeOps); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		migrations = append(migrations, m...)
	}

	// schema/ — has PG schema subfolders (public/, materialized/, etc.)
	if m, err := scanSchemaFolder(filepath.Join(dir, FolderSchema)); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		migrations = append(migrations, m...)
	}

	// logic/
	if m, err := scanSimpleFolder(filepath.Join(dir, FolderLogic), FolderLogic, TypeLogic); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		migrations = append(migrations, m...)
	}

	// permissions/
	if m, err := scanSimpleFolder(filepath.Join(dir, FolderPermissions), FolderPermissions, TypePermissions); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		migrations = append(migrations, m...)
	}

	return migrations, nil
}

// scanSchemaFolder reads schema/{pg_schema}/*.sql, setting PGSchema from the subfolder name.
func scanSchemaFolder(schemaDir string) ([]*Migration, error) {
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		return nil, err
	}

	var migrations []*Migration
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // skip files at the schema/ level — only subfolders matter
		}

		pgSchema := entry.Name()
		subDir := filepath.Join(schemaDir, pgSchema)
		files, err := os.ReadDir(subDir)
		if err != nil {
			return nil, fmt.Errorf("failed to read schema directory %q: %w", subDir, err)
		}

		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") {
				continue
			}

			filePath := filepath.Join(subDir, f.Name())
			content, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("failed to read %q: %w", filePath, err)
			}

			matches := simplePattern.FindStringSubmatch(f.Name())
			if matches == nil {
				continue
			}

			migrations = append(migrations, &Migration{
				Type:     TypeSchema,
				Name:     matches[1],
				PGSchema: pgSchema,
				Filename: filepath.Join(FolderSchema, pgSchema, f.Name()),
				FilePath: filePath,
				Checksum: ComputeChecksum(content),
				Content:  content,
			})
		}
	}

	return migrations, nil
}

// scanSimpleFolder reads *.sql files from a single folder.
func scanSimpleFolder(dir, folderName string, typ Type) ([]*Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var migrations []*Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read %q: %w", filePath, err)
		}

		checksum := ComputeChecksum(content)
		relFilename := filepath.Join(folderName, entry.Name())

		switch typ {
		case TypeOps:
			matches := opsPattern.FindStringSubmatch(entry.Name())
			if matches == nil {
				continue // skip files without timestamp prefix
			}
			migrations = append(migrations, &Migration{
				Type:     TypeOps,
				Version:  matches[1],
				Name:     strings.ReplaceAll(matches[2], "_", " "),
				Filename: relFilename,
				FilePath: filePath,
				Checksum: checksum,
				Content:  content,
			})

		case TypeLogic, TypePermissions:
			matches := simplePattern.FindStringSubmatch(entry.Name())
			if matches == nil {
				continue
			}
			migrations = append(migrations, &Migration{
				Type:     typ,
				Name:     matches[1],
				Filename: relFilename,
				FilePath: filePath,
				Checksum: checksum,
				Content:  content,
			})
		}
	}

	return migrations, nil
}

// discoverFlatLayout handles the legacy V__/S__/R__ prefix format.
func discoverFlatLayout(dir string) ([]*Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory %q: %w", dir, err)
	}

	var migrations []*Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		m, err := parseLegacyFile(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		if m != nil {
			migrations = append(migrations, m)
		}
	}

	return migrations, nil
}

func parseLegacyFile(dir, filename string) (*Migration, error) {
	filePath := filepath.Join(dir, filename)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read migration file %q: %w", filePath, err)
	}

	checksum := ComputeChecksum(content)

	if matches := legacyVersionedPattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeOps,
			Version:  matches[1],
			Name:     strings.ReplaceAll(matches[2], "_", " "),
			Filename: filename,
			FilePath: filePath,
			Checksum: checksum,
			Content:  content,
		}, nil
	}

	if matches := legacySchemaPattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeSchema,
			Name:     matches[1],
			PGSchema: "public", // legacy flat layout defaults to public schema
			Filename: filename,
			FilePath: filePath,
			Checksum: checksum,
			Content:  content,
		}, nil
	}

	if matches := legacyRepeatablePattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeLogic,
			Name:     matches[1],
			Filename: filename,
			FilePath: filePath,
			Checksum: checksum,
			Content:  content,
		}, nil
	}

	return nil, nil
}

// ComputeChecksum returns the SHA-256 hex digest of the given content.
func ComputeChecksum(content []byte) string {
	h := sha256.Sum256(content)
	return fmt.Sprintf("%x", h[:])
}
