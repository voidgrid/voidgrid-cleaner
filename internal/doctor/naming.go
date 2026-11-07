package doctor

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// Naming groups, in the order the doctor page lists them: the safe ones first.
var NamingGroups = []string{"capital letters only", "punctuation only", "part number only", "quality label only", "episode title changed", "movie title or year changed"}

var (
	qualitySuffix = regexp.MustCompile(`^(.*?)\s*-?\s*((?:HDTV|WEBDL|WEBRip|Bluray|Remux|DVD|SDTV|Raw-HD|Unknown)[-\w ]*)$`)
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
	var data fixData
	if json.Unmarshal([]byte(f.Data), &data) != nil {
		return NamingGroups[4]
	}
	o, n := data.OldName, data.NewName
	ot, oq := splitQuality(o)
	nt, nq := splitQuality(n)
	switch {
	case strings.EqualFold(o, n):
		return NamingGroups[0]
	case strings.EqualFold(notAlnum.ReplaceAllString(o, ""), notAlnum.ReplaceAllString(n, "")):
		return NamingGroups[1]
	case partNumber.ReplaceAllString(ot, "") == partNumber.ReplaceAllString(nt, "") && oq == nq:
		return NamingGroups[2]
	case strings.EqualFold(ot, nt) && oq != nq:
		return NamingGroups[3]
	case data.App == appRadarr:
		return NamingGroups[5]
	}
	return NamingGroups[4]
}
