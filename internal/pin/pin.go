// Package pin selects the newest image tag allowed by pinbumper labels.
//
// Two independent filters exist; a candidate must pass every filter that is set:
//
//   - pinbumper.range — npm/Renovate semver (Masterminds/semver, npm-compatible).
//     Only strict MAJOR.MINOR.PATCH tags are considered. latest, beta, rc, and
//     floating tags like "3" or "3.1" are ignored.
//   - pinbumper.include / pinbumper.exclude — regex against the full tag string
//     (WUD-style include, optional denylist).
//
// When range is set, the newest allowed tag is the highest semver.
// When only include/exclude is set, tags are compared with a version-aware
// sort (digit runs compared numerically, like GNU sort -V). The highest wins.
package pin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/Masterminds/semver/v3"
)

// Strict X.Y.Z semver, optional leading v, optional prerelease/build.
// Rejects floating "3", "3.1", "latest", "beta", "rc".
var strictSemver = regexp.MustCompile(`(?i)^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// Selector is the pin policy for one Compose service.
type Selector struct {
	Range   string
	Include string
	Exclude string

	constraint *semver.Constraints
	includeRE  *regexp.Regexp
	excludeRE  *regexp.Regexp
}

// New compiles range and regex labels. Empty fields are omitted.
func New(rangeSpec, include, exclude string) (Selector, error) {
	s := Selector{Range: strings.TrimSpace(rangeSpec), Include: include, Exclude: exclude}
	if s.Range != "" {
		c, err := semver.NewConstraint(s.Range)
		if err != nil {
			return Selector{}, fmt.Errorf("pinbumper.range %q: %w", s.Range, err)
		}
		s.constraint = c
	}
	if include != "" {
		re, err := regexp.Compile(include)
		if err != nil {
			return Selector{}, fmt.Errorf("pinbumper.include %q: %w", include, err)
		}
		s.includeRE = re
	}
	if exclude != "" {
		re, err := regexp.Compile(exclude)
		if err != nil {
			return Selector{}, fmt.Errorf("pinbumper.exclude %q: %w", exclude, err)
		}
		s.excludeRE = re
	}
	if s.Range == "" && include == "" {
		if exclude != "" {
			return Selector{}, fmt.Errorf("pinbumper.exclude requires pinbumper.range or pinbumper.include")
		}
		return Selector{}, fmt.Errorf("no pinbumper.range or pinbumper.include")
	}
	return s, nil
}

// Active reports whether any pinbumper label is set.
func (s Selector) Active() bool {
	return s.Range != "" || s.Include != "" || s.Exclude != ""
}

// SemverMode is true when a range is present (npm rules; ignore non-semver tags).
func (s Selector) SemverMode() bool {
	return s.constraint != nil
}

// Allows reports whether tag is a legal candidate.
func (s Selector) Allows(tag string) bool {
	if tag == "" {
		return false
	}
	if s.includeRE != nil && !s.includeRE.MatchString(tag) {
		return false
	}
	if s.excludeRE != nil && s.excludeRE.MatchString(tag) {
		return false
	}
	if s.constraint != nil {
		v, ok := parseStrict(tag)
		if !ok {
			return false
		}
		return s.constraint.Check(v)
	}
	return true
}

// Newest returns the highest allowed tag, or false if none match.
func (s Selector) Newest(tags []string) (string, bool) {
	var best string
	found := false
	for _, tag := range tags {
		if !s.Allows(tag) {
			continue
		}
		if !found || s.greater(tag, best) {
			best = tag
			found = true
		}
	}
	return best, found
}

// Choose returns the newest allowed tag and whether it differs from current.
// ok is false when there is no allowed tag or the pin is already newest
// (including semver-equal tags that differ only by a v prefix).
func (s Selector) Choose(current string, tags []string) (newest string, changed bool) {
	best, ok := s.Newest(tags)
	if !ok {
		return "", false
	}
	if best == current || semverEqual(best, current) {
		return best, false
	}
	return best, true
}

func (s Selector) greater(a, b string) bool {
	if s.constraint != nil {
		va, oka := parseStrict(a)
		vb, okb := parseStrict(b)
		if oka && okb {
			return va.GreaterThan(vb)
		}
	}
	return CompareVersionish(a, b) > 0
}

func parseStrict(tag string) (*semver.Version, bool) {
	if !strictSemver.MatchString(tag) {
		return nil, false
	}
	v, err := semver.NewVersion(tag)
	if err != nil {
		return nil, false
	}
	return v, true
}

func semverEqual(a, b string) bool {
	va, oka := parseStrict(a)
	vb, okb := parseStrict(b)
	if !oka || !okb {
		return false
	}
	return va.Equal(vb)
}

// CompareVersionish compares tags with a sort -V style tokenizer:
// alternating non-digit and digit runs; digits compared numerically.
// A longer equal-prefix token list is greater (1.0.0 > 1.0).
func CompareVersionish(a, b string) int {
	ta, tb := tokenize(a), tokenize(b)
	n := len(ta)
	if len(tb) < n {
		n = len(tb)
	}
	for i := 0; i < n; i++ {
		if c := ta[i].compare(tb[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(ta) < len(tb):
		return -1
	case len(ta) > len(tb):
		return 1
	default:
		return 0
	}
}

type vtoken struct {
	digits bool
	raw    string
	num    uint64
	okNum  bool
}

func (t vtoken) compare(o vtoken) int {
	if t.digits && o.digits {
		if t.okNum && o.okNum {
			switch {
			case t.num < o.num:
				return -1
			case t.num > o.num:
				return 1
			}
			// equal numerically: shorter raw (fewer leading zeros) wins? treat equal
			return 0
		}
	}
	return strings.Compare(t.raw, o.raw)
}

func tokenize(s string) []vtoken {
	if s == "" {
		return nil
	}
	var out []vtoken
	var b strings.Builder
	digit := unicode.IsDigit(rune(s[0]))
	flush := func() {
		if b.Len() == 0 {
			return
		}
		raw := b.String()
		tok := vtoken{digits: digit, raw: raw}
		if digit {
			if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
				tok.num = n
				tok.okNum = true
			}
		}
		out = append(out, tok)
		b.Reset()
	}
	for _, r := range s {
		d := unicode.IsDigit(r)
		if d != digit {
			flush()
			digit = d
		}
		b.WriteRune(r)
	}
	flush()
	return out
}
