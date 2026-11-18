package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// fakeTags records what the web asked Plex to change; failOn makes one item fail.
type fakeTags struct {
	calls  []string
	failOn string
	next   int
}

func (f *fakeTags) EditLabels(_ context.Context, section string, typ int, key string, add, remove []string) error {
	f.calls = append(f.calls, fmt.Sprintf("labels %s type %d item %s +%v -%v", section, typ, key, add, remove))
	if key == f.failOn {
		return errors.New("plex said no")
	}
	return nil
}

func (f *fakeTags) AddToCollection(_ context.Context, ck, key string) error {
	f.calls = append(f.calls, fmt.Sprintf("collection %s add %s", ck, key))
	if key == f.failOn {
		return errors.New("plex said no")
	}
	return nil
}

func (f *fakeTags) RemoveFromCollection(_ context.Context, ck, key string) error {
	f.calls = append(f.calls, fmt.Sprintf("collection %s remove %s", ck, key))
	return nil
}

func (f *fakeTags) DeleteCollection(_ context.Context, ck string) error {
	f.calls = append(f.calls, "collection "+ck+" delete")
	return nil
}

func (f *fakeTags) CreateCollection(_ context.Context, section string, typ int, title, first string) (string, error) {
	f.next++
	f.calls = append(f.calls, fmt.Sprintf("create %q in %s type %d from %s", title, section, typ, first))
	return fmt.Sprintf("90%d", f.next), nil
}

// tagApp is an admin session over the seeded library, with labels and collections in it:
// movie 10 is Momma and watch-together and in collection 500; movie 11 is Momma; show 20 has none.
func tagApp(t *testing.T) (post func(path string, v url.Values) *http.Response, read func(path string) string, s *Server, fake *fakeTags) {
	t.Helper()
	a, c, srv := newTestApp(t, 0)
	seedLibrary(t, srv)
	fake = &fakeTags{}
	srv.Tags = fake
	postForm(t, c, a.URL+"/login/local", url.Values{"email": {"admin@x"}, "password": {"pw"}})
	if err := srv.DB.ReplaceTags(context.Background(), storage.TagSnapshot{
		Labels:     []storage.SectionLabel{{SectionKey: "1", Title: "Momma"}, {SectionKey: "1", Title: "watch-together"}, {SectionKey: "1", Title: "Kometa"}, {SectionKey: "2", Title: "Momma"}},
		ItemLabels: []storage.ItemLabel{{RatingKey: "10", Label: "Momma"}, {RatingKey: "10", Label: "watch-together"}, {RatingKey: "11", Label: "Momma"}},
		Collections: []storage.PlexCollection{
			{Key: "500", SectionKey: "1", Title: "Alien Collection", Subtype: "movie", ItemCount: 1},
			{Key: "501", SectionKey: "1", Title: "Smart One", Subtype: "movie", Smart: true},
			{Key: "600", SectionKey: "2", Title: "TV Pile", Subtype: "show"},
		},
		Members: []storage.CollectionMember{{RatingKey: "10", CollectionKey: "500"}},
	}); err != nil {
		t.Fatal(err)
	}
	return func(path string, v url.Values) *http.Response { return postForm(t, c, a.URL+path, v) },
		func(path string) string { return body(t, get(t, c, a.URL+path)) }, srv, fake
}

func TestLibraryShowsTagControlsToAdmins(t *testing.T) {
	_, get, _, _ := tagApp(t)
	b := get("/library/1")
	for _, want := range []string{
		`id="bulk"`, `action="/library/1/bulk"`, `name="id" value="10"`, `data-tick-cards`,
		`<span class="chip">Momma<form`, `<span class="chip">watch-together<form`, `href="/library/1?collection=500">Alien Collection`,
		`<option value="500">Alien Collection</option>`, `<option value="Kometa">`, `name="label"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("library lacks %q", want)
		}
	}
	// The bar offers collections that take items by hand; the filter lists every one.
	bar := b[strings.Index(b, `id="bulk"`):]
	bar = bar[:strings.Index(bar, "</form>")]
	if strings.Contains(bar, "Smart One") || !strings.Contains(bar, "Alien Collection") || !strings.Contains(b, `<option value="501">Smart One</option>`) {
		t.Error("the bar offers a smart collection, or the filter lacks it")
	}
	// The filters: by label and by collection.
	for q, want := range map[string]string{
		"label=Momma":                "Bobs Movie, Unrequested",
		"label=watch-together":       "Bobs Movie",
		"label=Kometa":               "",
		"collection=500":             "Bobs Movie",
		"collection=501":             "",
		"label=Momma&collection=500": "Bobs Movie",
	} {
		b := get("/library/1?" + q)
		var got []string
		for _, title := range []string{"Bobs Movie", "Unrequested"} {
			if strings.Contains(b, `<span class="card-title">`+title+`</span>`) {
				got = append(got, title)
			}
		}
		if strings.Join(got, ", ") != want {
			t.Errorf("?%s: got %q want %q", q, strings.Join(got, ", "), want)
		}
	}
	if b := get("/library/1?label=Momma"); !strings.Contains(b, `<option value="Momma" selected>`) || !strings.Contains(b, `<details class="filters" open>`) {
		t.Error("the label filter isn't kept")
	}
}

func TestTagControlsAreAdminOnly(t *testing.T) {
	app, c, s := newTestApp(t, 200) // bob
	seedLibrary(t, s)
	s.Tags = &fakeTags{}
	get(t, c, app.URL+"/login/plex")
	get(t, c, app.URL+"/login/plex/callback")
	b := body(t, get(t, c, app.URL+"/library/1"))
	for _, no := range []string{`id="bulk"`, `class="tick"`, `class="chips"`, `name="collection"`} {
		if strings.Contains(b, no) {
			t.Errorf("a user sees %q", no)
		}
	}
	for _, p := range []string{"/library/1/bulk", "/library/item/10/label", "/library/item/10/collection/500/remove", "/labels/remove", "/collections/500/delete"} {
		if resp := postForm(t, c, app.URL+p, url.Values{"id": {"10"}, "action": {"label-add"}, "label": {"x"}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s: %d", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/labels", "/collections"} {
		if resp := get(t, c, app.URL+p); resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
}

func TestBulkLabels(t *testing.T) {
	post, _, s, fake := tagApp(t)
	ctx := context.Background()
	// Added in the library's own spelling, whatever case was typed, on every ticked item, and mirrored here.
	resp := post("/library/1/bulk", url.Values{"id": {"10", "11"}, "action": {"label-add"}, "label": {"  WATCH-Together "}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	want := []string{
		"labels 1 type 1 item 10 +[watch-together] -[]",
		"labels 1 type 1 item 11 +[watch-together] -[]",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls %v", fake.calls)
	}
	if got, _ := s.DB.ItemsWithLabel(ctx, "1", "watch-together"); !reflect.DeepEqual(got, []string{"11", "10"}) && !reflect.DeepEqual(got, []string{"10", "11"}) {
		t.Fatalf("mirror %v", got)
	}
	// A brand new name is used as typed, and joins the library's list.
	fake.calls = nil
	post("/library/1/bulk", url.Values{"id": {"10"}, "action": {"label-add"}, "label": {"Christmas Movies"}})
	if names, _ := s.DB.LabelNames(ctx, "1"); !strings.Contains(strings.Join(names, "|"), "Christmas Movies") {
		t.Fatalf("names %v", names)
	}
	// Removing takes the label off the ticked items only; the name stays listed, as in Plex.
	fake.calls = nil
	if resp := post("/library/1/bulk", url.Values{"id": {"11"}, "action": {"label-remove"}, "label": {"momma"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(fake.calls, []string{"labels 1 type 1 item 11 +[] -[Momma]"}) {
		t.Fatalf("remove calls %v", fake.calls)
	}
	if got, _ := s.DB.ItemsWithLabel(ctx, "1", "Momma"); !reflect.DeepEqual(got, []string{"10"}) {
		t.Fatalf("momma %v", got)
	}
}

func TestBulkRefusals(t *testing.T) {
	post, _, s, fake := tagApp(t)
	many := make([]string, maxBulk+1)
	for i := range many {
		many[i] = fmt.Sprint("x", i)
	}
	for name, c := range map[string]struct {
		path string
		v    url.Values
	}{
		"nothing ticked":            {"/library/1/bulk", url.Values{"action": {"label-add"}, "label": {"x"}}},
		"too many":                  {"/library/1/bulk", url.Values{"id": many, "action": {"label-add"}, "label": {"x"}}},
		"an item from elsewhere":    {"/library/1/bulk", url.Values{"id": {"10", "20"}, "action": {"label-add"}, "label": {"x"}}},
		"an unknown item":           {"/library/1/bulk", url.Values{"id": {"nope"}, "action": {"label-add"}, "label": {"x"}}},
		"no action":                 {"/library/1/bulk", url.Values{"id": {"10"}}},
		"no label":                  {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"label-add"}, "label": {"  "}}},
		"a comma in a label":        {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"label-add"}, "label": {"a,b"}}},
		"a long label":              {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"label-add"}, "label": {strings.Repeat("x", 101)}}},
		"no collection":             {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"collection-add"}}},
		"a collection elsewhere":    {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"collection-add"}, "collection": {"600"}}},
		"a smart collection":        {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"collection-add"}, "collection": {"501"}}},
		"a new collection, no name": {"/library/1/bulk", url.Values{"id": {"10"}, "action": {"collection-new"}}},
	} {
		if resp := post(c.path, c.v); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a refused request reached Plex: %v", fake.calls)
	}
	if rows, _ := s.DB.Labels(context.Background()); len(rows) != 4 {
		t.Fatalf("the label list changed: %+v", rows)
	}
}

// One item failing doesn't stop the rest; the page says which, and only the done ones are mirrored.
func TestBulkPartialFailure(t *testing.T) {
	post, _, s, fake := tagApp(t)
	fake.failOn = "10"
	resp := post("/library/1/bulk", url.Values{"id": {"10", "11"}, "action": {"label-add"}, "label": {"Christmas"}})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("%d", resp.StatusCode)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("the second item was skipped: %v", fake.calls)
	}
	if got, _ := s.DB.ItemsWithLabel(context.Background(), "1", "Christmas"); !reflect.DeepEqual(got, []string{"11"}) {
		t.Fatalf("mirror %v", got)
	}
	if b := body(t, resp); !strings.Contains(b, "1 of 2 failed") || !strings.Contains(b, "10: plex said no") {
		t.Fatalf("%s", b)
	}
}

func TestBulkCollections(t *testing.T) {
	post, _, s, fake := tagApp(t)
	ctx := context.Background()
	if resp := post("/library/1/bulk", url.Values{"id": {"10", "11"}, "action": {"collection-add"}, "collection": {"500"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	if got, _ := s.DB.ItemsInCollection(ctx, "500"); len(got) != 2 {
		t.Fatalf("members %v", got)
	}
	if c, _, _ := s.DB.Collection(ctx, "500"); c.ItemCount != 2 { // 10 was already in
		t.Fatalf("count %d", c.ItemCount)
	}
	if resp := post("/library/1/bulk", url.Values{"id": {"11"}, "action": {"collection-remove"}, "collection": {"500"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if got, _ := s.DB.ItemsInCollection(ctx, "500"); !reflect.DeepEqual(got, []string{"10"}) {
		t.Fatalf("members %v", got)
	}
	// A new collection is made from the first ticked item, and the rest are added.
	fake.calls = nil
	if resp := post("/library/1/bulk", url.Values{"id": {"10", "11"}, "action": {"collection-new"}, "title": {"Movie Night, Friday"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("new: %d", resp.StatusCode)
	}
	want := []string{`create "Movie Night, Friday" in 1 type 1 from 10`, "collection 901 add 11"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("calls %v", fake.calls)
	}
	c, found, _ := s.DB.Collection(ctx, "901")
	if got, _ := s.DB.ItemsInCollection(ctx, "901"); !found || c.Title != "Movie Night, Friday" || c.ItemCount != 2 || len(got) != 2 {
		t.Fatalf("%+v %v %v", c, found, got)
	}
}

func TestCardLabelsAndCollections(t *testing.T) {
	post, _, s, fake := tagApp(t)
	ctx := context.Background()
	if resp := post("/library/item/10/label", url.Values{"op": {"remove"}, "name": {"watch-together"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if resp := post("/library/item/10/label", url.Values{"op": {"add"}, "name": {"classic"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	want := []string{"labels 1 type 1 item 10 +[] -[watch-together]", "labels 1 type 1 item 10 +[classic] -[]"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("%v", fake.calls)
	}
	if got, _ := s.DB.ItemsWithLabel(ctx, "1", "watch-together"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// A show takes the show type.
	fake.calls = nil
	post("/library/item/20/label", url.Values{"op": {"add"}, "name": {"Momma"}})
	if !reflect.DeepEqual(fake.calls, []string{"labels 2 type 2 item 20 +[Momma] -[]"}) {
		t.Fatalf("%v", fake.calls)
	}
	for name, v := range map[string]url.Values{"no op": {"name": {"x"}}, "bad op": {"op": {"drop"}, "name": {"x"}}, "no name": {"op": {"add"}}} {
		if resp := post("/library/item/10/label", v); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp := post("/library/item/nope/label", url.Values{"op": {"add"}, "name": {"x"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown item: %d", resp.StatusCode)
	}
	// Out of a collection, but not a smart one and not one from another library.
	fake.calls = nil
	if resp := post("/library/item/10/collection/500/remove", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("collection remove: %d", resp.StatusCode)
	}
	if got, _ := s.DB.ItemsInCollection(ctx, "500"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if resp := post("/library/item/10/collection/501/remove", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("smart: %d", resp.StatusCode)
	}
	if resp := post("/library/item/10/collection/600/remove", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other library: %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(fake.calls, []string{"collection 500 remove 10"}) {
		t.Fatalf("%v", fake.calls)
	}
}

func TestLabelsPage(t *testing.T) {
	post, get, s, fake := tagApp(t)
	b := get("/labels")
	for _, want := range []string{
		"<td>Movies</td>", "<td>Kometa</td>", `<span class="muted">0, unused</span>`,
		`<a href="/library/1?label=Momma">2</a>`, `remove Momma from 2 items`, `value="watch-together"`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("labels page lacks %q", want)
		}
	}
	if strings.Contains(b, "remove Kometa") {
		t.Error("an unused label offers a removal")
	}
	// Remove from everywhere takes it off every item that has it.
	if resp := post("/labels/remove", url.Values{"section": {"1"}, "label": {"Momma"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	want := []string{"labels 1 type 1 item 10 +[] -[Momma]", "labels 1 type 1 item 11 +[] -[Momma]"}
	slices.Sort(fake.calls)
	if !slices.Equal(fake.calls, want) {
		t.Fatalf("%v", fake.calls)
	}
	if got, _ := s.DB.ItemsWithLabel(context.Background(), "1", "Momma"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// The show in the other library keeps its Momma: the removal was for one library.
	if resp := post("/labels/remove", url.Values{"section": {"1"}, "label": {"Kometa"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("nothing carries it: %d", resp.StatusCode)
	}
}

func TestCollectionsPage(t *testing.T) {
	post, get, s, fake := tagApp(t)
	b := get("/collections")
	for _, want := range []string{
		"<td>Alien Collection</td>", `<a href="/library/1?collection=500">1</a>`, `delete Alien Collection, keep its 1 item`,
		`<td class="muted">smart</td>`, "<td>TV Pile</td>",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("collections page lacks %q", want)
		}
	}
	if resp := post("/collections/500/delete", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if !reflect.DeepEqual(fake.calls, []string{"collection 500 delete"}) {
		t.Fatalf("%v", fake.calls)
	}
	if _, found, _ := s.DB.Collection(context.Background(), "500"); found {
		t.Fatal("still stored")
	}
	// The movie is still in the library: only the collection went.
	if b := get("/library/1"); !strings.Contains(b, `<span class="card-title">Bobs Movie</span>`) {
		t.Fatal("the movie went with it")
	}
	if resp := post("/collections/nope/delete", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
}
