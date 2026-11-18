package storage

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func tagFixture(t *testing.T) *DB {
	t.Helper()
	db := openTest(t)
	ctx := context.Background()
	if err := db.ReplaceLibrary(ctx, []Section{{Key: "1", Title: "Movies", Type: "movie"}, {Key: "2", Title: "TV", Type: "show"}}, []Media{
		{RatingKey: "10", SectionKey: "1", Type: "movie", Title: "a"},
		{RatingKey: "11", SectionKey: "1", Type: "movie", Title: "b"},
		{RatingKey: "12", SectionKey: "1", Type: "movie", Title: "c"},
		{RatingKey: "20", SectionKey: "2", Type: "show", Title: "s"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceTags(ctx, TagSnapshot{
		Labels: []SectionLabel{{"1", "Momma"}, {"1", "watch-together"}, {"1", "Kometa"}, {"2", "Momma"}},
		ItemLabels: []ItemLabel{
			{"10", "Momma"}, {"10", "watch-together"}, {"11", "momma"}, // Plex matches names without regard to case
			{"20", "Momma"},
		},
		Collections: []PlexCollection{
			{Key: "500", SectionKey: "1", Title: "Alien Collection", Subtype: "movie", ItemCount: 2},
			{Key: "501", SectionKey: "1", Title: "Top", Subtype: "movie", Smart: true, ItemCount: 1},
		},
		Members: []CollectionMember{{"10", "500"}, {"11", "500"}, {"12", "501"}},
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLabelSummaryCountsPerLibrary(t *testing.T) {
	db := tagFixture(t)
	rows, err := db.Labels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s/%s=%d", r.SectionTitle, r.Title, r.Items))
	}
	// Kometa has no items but stays listed, as it does in Plex; "momma" on item 11 counts for Momma.
	want := "Movies/Kometa=0 Movies/Momma=2 Movies/watch-together=1 TV/Momma=1"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
	names, _ := db.LabelNames(context.Background(), "1")
	if !reflect.DeepEqual(names, []string{"Kometa", "Momma", "watch-together"}) {
		t.Fatalf("%v", names)
	}
}

func TestLibraryItemsCarryTagsAndFilterByThem(t *testing.T) {
	db := tagFixture(t)
	ctx := context.Background()
	admin := User{IsAdmin: true}
	items, err := db.LibraryItems(ctx, admin, "1", LibraryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]LibraryItem{}
	for _, it := range items {
		byKey[it.RatingKey] = it
	}
	if got := byKey["10"].Labels; !reflect.DeepEqual(got, []string{"Momma", "watch-together"}) {
		t.Fatalf("labels %v", got)
	}
	if got := byKey["10"].Collections; !reflect.DeepEqual(got, []CollectionRef{{"500", "Alien Collection"}}) {
		t.Fatalf("collections %v", got)
	}
	if len(byKey["12"].Labels) != 0 || byKey["12"].Collections[0].Title != "Top" {
		t.Fatalf("%+v", byKey["12"])
	}
	keys := func(f LibraryFilter) string {
		items, err := db.LibraryItems(ctx, admin, "1", f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.RatingKey)
		}
		return strings.Join(out, " ")
	}
	for name, c := range map[string]struct {
		f    LibraryFilter
		want string
	}{
		"label":                {LibraryFilter{Label: "Momma"}, "10 11"},
		"label, other case":    {LibraryFilter{Label: "WATCH-TOGETHER"}, "10"},
		"label nobody has":     {LibraryFilter{Label: "Kometa"}, ""},
		"collection":           {LibraryFilter{Collection: "500"}, "10 11"},
		"label and collection": {LibraryFilter{Label: "watch-together", Collection: "500"}, "10"},
		"label and a filter":   {LibraryFilter{Label: "Momma", Search: "b"}, "11"},
	} {
		if got := keys(c.f); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestMirrorOfPlexEdits(t *testing.T) {
	db := tagFixture(t)
	ctx := context.Background()
	if err := db.AddItemLabel(ctx, "1", "12", "Christmas"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveItemLabel(ctx, "10", "momma"); err != nil { // case doesn't matter
		t.Fatal(err)
	}
	if got, _ := db.ItemsWithLabel(ctx, "1", "Momma"); !reflect.DeepEqual(got, []string{"11"}) {
		t.Fatalf("momma: %v", got)
	}
	if got, _ := db.ItemsWithLabel(ctx, "1", "Christmas"); !reflect.DeepEqual(got, []string{"12"}) {
		t.Fatalf("christmas: %v", got)
	}
	// A new name appears in the library's list; a name that loses its last item stays, as in Plex.
	names, _ := db.LabelNames(ctx, "1")
	if !reflect.DeepEqual(names, []string{"Christmas", "Kometa", "Momma", "watch-together"}) {
		t.Fatalf("%v", names)
	}
	// Collections: counts follow, and a repeat changes nothing.
	for i := 0; i < 2; i++ {
		if err := db.AddToCollection(ctx, "501", "10"); err != nil {
			t.Fatal(err)
		}
	}
	c, found, _ := db.Collection(ctx, "501")
	if !found || c.ItemCount != 2 || !c.Smart {
		t.Fatalf("%+v %v", c, found)
	}
	if err := db.RemoveFromCollection(ctx, "501", "10"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveFromCollection(ctx, "501", "10"); err != nil { // already out
		t.Fatal(err)
	}
	if c, _, _ := db.Collection(ctx, "501"); c.ItemCount != 1 {
		t.Fatalf("count %d", c.ItemCount)
	}
	if err := db.AddCollection(ctx, PlexCollection{Key: "900", SectionKey: "1", Title: "New", Subtype: "movie", ItemCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCollection(ctx, "500"); err != nil {
		t.Fatal(err)
	}
	all, _ := db.AllCollections(ctx)
	var titles []string
	for _, c := range all {
		titles = append(titles, c.Title)
	}
	if !reflect.DeepEqual(titles, []string{"New", "Top"}) {
		t.Fatalf("%v", titles)
	}
	if got, _ := db.ItemsInCollection(ctx, "500"); len(got) != 0 {
		t.Fatalf("a deleted collection still has members: %v", got)
	}
	// The items themselves are untouched.
	items, _ := db.LibraryItems(ctx, User{IsAdmin: true}, "1", LibraryFilter{})
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
}

func TestMediaTypesOnlyOfThisLibrary(t *testing.T) {
	db := tagFixture(t)
	got, err := db.MediaTypes(context.Background(), "1", []string{"10", "20", "nope"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{"10": "movie"}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := db.MediaTypes(context.Background(), "1", nil); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
}

// A sync replaces what was stored, so what Plex dropped is gone.
func TestReplaceTagsDropsWhatPlexDropped(t *testing.T) {
	db := tagFixture(t)
	ctx := context.Background()
	if err := db.ReplaceTags(ctx, TagSnapshot{Labels: []SectionLabel{{"1", "Momma"}}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.Labels(ctx)
	all, _ := db.AllCollections(ctx)
	if len(rows) != 1 || rows[0].Items != 0 || len(all) != 0 {
		t.Fatalf("%+v %+v", rows, all)
	}
}
