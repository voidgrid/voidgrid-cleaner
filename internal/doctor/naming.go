package doctor

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Naming groups, in the order the doctor page lists them: the safe ones first.
var NamingGroups = []string{"capital letters only", "punctuation only", "folder or path only", "part number only", "quality label only", "episode title changed", "movie title or year changed"}

var (
	// The label is a quality word, its resolution and known extras, at the very
	// end. Anything looser takes a title's own words ("The Unknown Soldier") for
	// part of the label, and a title change for a harmless one.
	qualitySuffix = regexp.MustCompile(`^(.*?)\s*-?\s*\b((?:HDTV|WEBDL|WEBRip|Bluray|Remux|DVD|SDTV|Raw-HD|Unknown)(?:-\w+)?(?: (?:Remux|Proper|Repack|REAL|v\d+))*)$`)
	partNumber    = regexp.MustCompile(`\s*\(\d+\)$`)
	notAlnum      = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

func splitQuality(name string) (title, quality string) {
	s := strings.TrimSuffix(name, path.Ext(name))
	if m := qualitySuffix.FindStringSubmatch(s); m != nil {
		return strings.Trim(m[1], " -"), m[2]
	}
	return s, ""
}

// NamingGroup says what kind of rename a naming finding is.
func NamingGroup(f storage.Finding) string {
	const (
		capitals, punctuation, location, part, quality, episode, movie = 0, 1, 2, 3, 4, 5, 6
	)
	var data fixData
	if json.Unmarshal([]byte(f.Data), &data) != nil {
		return NamingGroups[episode]
	}
	o, n := data.OldName, data.NewName
	ot, oq := splitQuality(o)
	nt, nq := splitQuality(n)
	switch {
	case o == n:
		// The file name stays; the app wants it somewhere else.
		return NamingGroups[location]
	case strings.EqualFold(o, n):
		return NamingGroups[capitals]
	case strings.EqualFold(notAlnum.ReplaceAllString(o, ""), notAlnum.ReplaceAllString(n, "")):
		return NamingGroups[punctuation]
	case partNumber.ReplaceAllString(ot, "") == partNumber.ReplaceAllString(nt, "") && oq == nq:
		return NamingGroups[part]
	case strings.EqualFold(ot, nt) && oq != nq:
		return NamingGroups[quality]
	case data.App == appRadarr:
		return NamingGroups[movie]
	}
	return NamingGroups[episode]
}

// renameDetail is what a naming finding says under its title.
func renameDetail(d fixData) string {
	s := "Would become " + cmp.Or(d.NewPath, d.NewName)
	if note := changeNote(d); note != "" {
		s += ". " + note
	}
	return s
}

// changeNote names what differs, so a swapped apostrophe or a moved folder
// doesn't look like nothing. It's empty when there is no more to say.
func changeNote(d fixData) string {
	o, n := []rune(d.OldName), []rune(d.NewName)
	if d.OldName == d.NewName {
		if d.OldPath == d.NewPath || d.OldPath == "" {
			return ""
		}
		return "Only the folder or path changes: the file name stays."
	}
	pre := 0
	for pre < len(o) && pre < len(n) && o[pre] == n[pre] {
		pre++
	}
	suf := 0
	for suf < len(o)-pre && suf < len(n)-pre && o[len(o)-1-suf] == n[len(n)-1-suf] {
		suf++
	}
	return "Changes " + quoteRunes(o[pre:len(o)-suf]) + " to " + quoteRunes(n[pre:len(n)-suf]) + "."
}

// quoteRunes shows text with its code points, since look-alikes are the point.
func quoteRunes(r []rune) string {
	if len(r) == 0 {
		return "nothing"
	}
	if len(r) > 12 {
		r = r[:12]
	}
	var b strings.Builder
	for i, c := range r {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%q (U+%04X)", c, c)
	}
	return b.String()
}
