package dump

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// LogicObject identifies one function, procedure, or view destined for logic/.
//
// Identity carries the function's argument list (as returned by
// pg_get_function_identity_arguments) and is empty for views. It is the only
// thing that distinguishes two overloads of the same function name.
type LogicObject struct {
	Schema   string
	Name     string
	Identity string
}

// BaseFileName is the filename a logic object gets when nothing collides with
// it: public objects keep their bare name, everything else is schema-prefixed.
func (o LogicObject) BaseFileName() string {
	if o.Schema == "public" {
		return o.Name
	}
	return o.Schema + "_" + o.Name
}

var nonFilenameChars = regexp.MustCompile(`[^a-z0-9]+`)

// argSlug renders an argument list into a short, filename-safe, deterministic
// suffix: "IN p_start_date date" -> "in_p_start_date_date", "" -> "noargs".
//
// Identity may be either a bare argument list or a full "name(args)"
// signature; only the arguments distinguish overloads, so a wrapping name is
// stripped to keep the suffix from repeating the name already in the prefix.
func argSlug(identity string) string {
	if open := strings.Index(identity, "("); open >= 0 && strings.HasSuffix(identity, ")") {
		identity = identity[open+1 : len(identity)-1]
	}
	s := nonFilenameChars.ReplaceAllString(strings.ToLower(identity), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "noargs"
	}
	return s
}

// ResolveLogicFileNames maps every logic object to a distinct filename.
//
// Overloaded functions share a name, so the naive schema+name scheme writes
// them all to one path and every overload but the last is silently lost from
// the baseline. Colliding objects are disambiguated by their argument list;
// objects with no collision keep the short, stable name.
//
// The returned map is keyed by schema, name, and identity so callers can look
// up the filename for the exact object they hold.
func ResolveLogicFileNames(objects []LogicObject) map[LogicObject]string {
	byBase := make(map[string][]LogicObject)
	for _, o := range objects {
		byBase[o.BaseFileName()] = append(byBase[o.BaseFileName()], o)
	}

	names := make(map[LogicObject]string, len(objects))
	for base, group := range byBase {
		if len(group) == 1 {
			names[group[0]] = base + ".sql"
			continue
		}
		// Deterministic order so the same database always dumps the same
		// filenames, even if the caller's iteration order changes.
		sort.Slice(group, func(i, j int) bool { return group[i].Identity < group[j].Identity })
		used := make(map[string]int, len(group))
		for _, o := range group {
			candidate := fmt.Sprintf("%s__%s", base, argSlug(o.Identity))
			if n := used[candidate]; n > 0 {
				candidate = fmt.Sprintf("%s_%d", candidate, n+1)
			}
			used[candidate]++
			names[o] = candidate + ".sql"
		}
	}
	return names
}
