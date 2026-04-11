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
	// TypeVersioned migrations run once, tracked by version.
	TypeVersioned Type = iota
	// TypeRepeatable migrations re-run when their content changes.
	TypeRepeatable
	// TypeSchema migrations are declarative desired-state files, auto-diffed against the live database.
	TypeSchema
)

// String returns the type name for display and database storage.
func (t Type) String() string {
	switch t {
	case TypeVersioned:
		return "versioned"
	case TypeRepeatable:
		return "repeatable"
	case TypeSchema:
		return "schema"
	default:
		return "unknown"
	}
}

// Migration represents a single migration file.
type Migration struct {
	Type     Type
	Version  string // Timestamp prefix for versioned; empty for repeatable/schema.
	Name     string // Human-readable name derived from filename.
	Filename string // Just the filename (no directory).
	FilePath string // Absolute path on disk.
	Checksum string // SHA-256 hex digest of file content.
	Content  []byte // Raw file content.
}

var (
	versionedPattern  = regexp.MustCompile(`^V(\d+)__(.+)\.sql$`)
	repeatablePattern = regexp.MustCompile(`^R__(.+)\.sql$`)
	schemaPattern     = regexp.MustCompile(`^S__(.+)\.sql$`)
)

// Discover scans a directory for migration files and returns them sorted:
// versioned by version, then schema alphabetically, then repeatable alphabetically.
func Discover(dir string) ([]*Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory %q: %w", dir, err)
	}

	var migrations []*Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		m, err := parseFile(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		if m != nil {
			migrations = append(migrations, m)
		}
	}

	sort.Slice(migrations, func(i, j int) bool {
		if migrations[i].Type != migrations[j].Type {
			return migrations[i].Type < migrations[j].Type
		}
		if migrations[i].Type == TypeVersioned {
			return migrations[i].Version < migrations[j].Version
		}
		return migrations[i].Name < migrations[j].Name
	})

	return migrations, nil
}

func parseFile(dir, filename string) (*Migration, error) {
	filePath := filepath.Join(dir, filename)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read migration file %q: %w", filePath, err)
	}

	checksum := ComputeChecksum(content)

	if matches := versionedPattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeVersioned,
			Version:  matches[1],
			Name:     strings.ReplaceAll(matches[2], "_", " "),
			Filename: filename,
			FilePath: filePath,
			Checksum: checksum,
			Content:  content,
		}, nil
	}

	if matches := schemaPattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeSchema,
			Name:     matches[1],
			Filename: filename,
			FilePath: filePath,
			Checksum: checksum,
			Content:  content,
		}, nil
	}

	if matches := repeatablePattern.FindStringSubmatch(filename); matches != nil {
		return &Migration{
			Type:     TypeRepeatable,
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
