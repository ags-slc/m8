package dump

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
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

// maxSlugLen caps the readable part of a disambiguated filename. A signature
// with a dozen named arguments would otherwise push the path past what some
// filesystems accept. Truncating is safe because a slug that is no longer unique
// after truncation collides, and a collision is what pulls in the hash.
const maxSlugLen = 64

// argSlug renders an argument list into a short, filename-safe, deterministic
// suffix: "IN p_start_date date" -> "in_p_start_date_date", "" -> "noargs".
//
// Identity may be either a bare argument list or a full "name(args)"
// signature; only the arguments distinguish overloads, so a wrapping name is
// stripped to keep the suffix from repeating the name already in the prefix.
//
// It is deliberately LOSSY -- "text" and "text[]" both slug to "text" -- because
// it is a readability aid, not an identity. Uniqueness comes from objectHash,
// which ResolveLogicFileNames appends whenever two objects land on one slug.
func argSlug(identity string) string {
	if open := strings.Index(identity, "("); open >= 0 && strings.HasSuffix(identity, ")") {
		identity = identity[open+1 : len(identity)-1]
	}
	s := nonFilenameChars.ReplaceAllString(strings.ToLower(identity), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "noargs"
	}
	if len(s) > maxSlugLen {
		s = strings.Trim(s[:maxSlugLen], "_")
	}
	return s
}

// objectHash is a short discriminator derived from everything that
// distinguishes one logic object from another, plus a round number so a second
// pass over an unlucky collision produces a different suffix. Length-prefixed so
// ("ab", "c") and ("a", "bc") cannot hash alike, and truncated to 48 bits --
// ample for the handful of objects that ever share a path, and short enough to
// keep the filename readable.
func objectHash(o LogicObject, round int) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d;", round)
	for _, part := range []string{o.Schema, o.Name, o.Identity} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

// ResolveLogicFileNames maps every logic object to a distinct filename.
//
// Overloaded functions share a name, so the naive schema+name scheme writes
// them all to one path and every overload but the last is silently lost from
// the baseline. Colliding objects are disambiguated by their argument list;
// objects with no collision keep the short, stable name.
//
// A name is a pure function of the object and of which other objects it collides
// with. Nothing depends on position:
//
//   - The disambiguator used to be an ordinal (__2, __3), which made the
//     filename depend on where an object fell in a sorted group. Removing one
//     overload then handed its path to a DIFFERENT overload -- the file stayed,
//     its contents silently became another function. A hash of the object
//     cannot do that: a removal can shorten a surviving name, never re-point an
//     existing one.
//   - The ordinal was also wrong. `used` was incremented against the *rewritten*
//     candidate, so a slug shared by three objects produced __2 twice and one of
//     them was overwritten -- the exact silent loss this function exists to
//     prevent.
//   - The sort that made the ordinal reproducible keyed on Identity alone and
//     was not stable, so objects with equal Identity (two views, both "")
//     swapped filenames from run to run. There is no ordinal now, so there is
//     nothing to sort.
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
		// Which slugs collide is a property of the set, not of any object's
		// position in it. argSlug is lossy -- f(text) and f(text[]) both slug to
		// "text" -- so this is what decides who needs a hash.
		slugCount := make(map[string]int, len(group))
		for _, o := range group {
			slugCount[argSlug(o.Identity)]++
		}
		for _, o := range group {
			slug := argSlug(o.Identity)
			if slugCount[slug] > 1 {
				// argSlug collapses every run of non-[a-z0-9] to a single "_",
				// so a slug can never contain "__" and base__slug can never be
				// mistaken for base__slug__hash.
				slug += "__" + objectHash(o, 0)
			}
			names[o] = base + "__" + slug + ".sql"
		}
	}

	// Everything above reasons WITHIN one base group, which is not enough. A
	// base can itself contain "__", because BaseFileName is Schema + "_" + Name:
	// the overload app.x(text) lands on app_x__text.sql, and so does the
	// unrelated helper app_x."_text"(), whose base IS app_x__text. And on a
	// case-insensitive filesystem -- APFS, NTFS -- Foo.sql and foo.sql are one
	// file however distinct the map keys are. Either way one object silently
	// overwrites the other, which is the failure this whole function exists to
	// prevent, so uniqueness is settled once, globally, over the finished set.
	deduplicate(names)
	return names
}

// deduplicate gives every object still sharing a path (compared case-folded, as
// a case-insensitive filesystem would) a hash suffix. Bounded: each round hashes
// with a different seed, and one round has always been enough -- the cap exists
// so an adversarial input cannot spin here.
func deduplicate(names map[LogicObject]string) {
	for round := 0; round < 4; round++ {
		byPath := make(map[string][]LogicObject, len(names))
		for o, n := range names {
			key := strings.ToLower(n)
			byPath[key] = append(byPath[key], o)
		}
		collided := false
		for _, group := range byPath {
			if len(group) < 2 {
				continue
			}
			collided = true
			for _, o := range group {
				names[o] = strings.TrimSuffix(names[o], ".sql") + "__" + objectHash(o, round) + ".sql"
			}
		}
		if !collided {
			return
		}
	}
}
