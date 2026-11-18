package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// TagWriter changes Plex's labels and collections.
type TagWriter interface {
	EditLabels(ctx context.Context, sectionKey string, typ int, ratingKey string, add, remove []string) error
	AddToCollection(ctx context.Context, collectionKey, ratingKey string) error
	RemoveFromCollection(ctx context.Context, collectionKey, ratingKey string) error
	DeleteCollection(ctx context.Context, collectionKey string) error
	CreateCollection(ctx context.Context, sectionKey string, typ int, title, ratingKey string) (string, error)
}

// maxBulk caps how many items one request may change; each is a call to Plex.
const maxBulk = 500

func plexType(mediaType string) int {
	if mediaType == "show" {
		return plex.TypeShow
	}
	return plex.TypeMovie
}

// cleanName checks a typed label or collection name.
func cleanName(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("type a name")
	case len([]rune(s)) > 100:
		return "", errors.New("that name is over 100 characters")
	case strings.ContainsRune(s, ','):
		return "", errors.New("a label name can't hold a comma")
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return "", errors.New("that name holds a control character")
	}
	return s, nil
}

// libraryLabel returns the library's own spelling of a label if it has one: Plex matches names
// without regard to case, and one spelling keeps the list tidy.
func (s *Server) libraryLabel(ctx context.Context, sectionKey, name string) (string, error) {
	names, err := s.DB.LabelNames(ctx, sectionKey)
	if err != nil {
		return "", err
	}
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return n, nil
		}
	}
	return name, nil
}

// changeLabel adds or removes a label on one item in Plex, then mirrors it here.
func (s *Server) changeLabel(ctx context.Context, sectionKey, mediaType, ratingKey, label string, add bool) error {
	if add {
		if err := s.Tags.EditLabels(ctx, sectionKey, plexType(mediaType), ratingKey, []string{label}, nil); err != nil {
			return err
		}
		return s.DB.AddItemLabel(ctx, sectionKey, ratingKey, label)
	}
	if err := s.Tags.EditLabels(ctx, sectionKey, plexType(mediaType), ratingKey, nil, []string{label}); err != nil {
		return err
	}
	return s.DB.RemoveItemLabel(ctx, ratingKey, label)
}

// backToList returns to the library with the filters it had.
func backToList(w http.ResponseWriter, r *http.Request, section string) {
	dest := url.URL{Path: "/library/" + section}
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && strings.HasPrefix(ref.Path, "/library/") {
		q := ref.Query()
		q.Del("limit")
		q.Del("offset")
		dest.RawQuery = q.Encode()
	}
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
}

// tagsOff answers when no Plex writer is wired in.
func (s *Server) tagsOff(w http.ResponseWriter) bool {
	if s.Tags == nil {
		http.Error(w, "labels and collections aren't available", http.StatusServiceUnavailable)
		return true
	}
	return false
}

// bulk applies one change to every ticked item of a library: a label added or removed, or the
// items put in or taken out of a collection, or put in a new one.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	if s.tagsOff(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	section := chi.URLParam(r, "key")
	var ids []string
	seen := map[string]bool{}
	for _, id := range r.PostForm["id"] {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "tick something first", http.StatusBadRequest)
		return
	}
	if len(ids) > maxBulk {
		http.Error(w, fmt.Sprintf("that's %d items; change at most %d at a time", len(ids), maxBulk), http.StatusBadRequest)
		return
	}
	types, err := s.DB.MediaTypes(r.Context(), section, ids)
	if err != nil {
		log.Printf("web: media types: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(types) != len(ids) {
		http.Error(w, "some of those aren't in this library", http.StatusBadRequest)
		return
	}
	// Past the checks the batch runs to the end even if the browser goes away; half a batch is worse.
	ctx := context.WithoutCancel(r.Context())
	action := r.PostFormValue("action")
	var detail string
	var failures []string
	fail := func(id string, err error) { failures = append(failures, fmt.Sprintf("%s: %v", id, err)) }

	switch action {
	case "label-add", "label-remove":
		name, err := cleanName(r.PostFormValue("label"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if name, err = s.libraryLabel(ctx, section, name); err != nil {
			log.Printf("web: label names: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		detail = name
		for _, id := range ids {
			if err := s.changeLabel(ctx, section, types[id], id, name, action == "label-add"); err != nil {
				fail(id, err)
			}
		}
	case "collection-add", "collection-remove":
		c, found, err := s.DB.Collection(ctx, r.PostFormValue("collection"))
		if err != nil {
			log.Printf("web: collection: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found || c.SectionKey != section {
			http.Error(w, "pick a collection in this library", http.StatusBadRequest)
			return
		}
		if c.Smart {
			http.Error(w, "a smart collection builds itself from a filter, so items can't be added or removed by hand", http.StatusBadRequest)
			return
		}
		detail = c.Title
		for _, id := range ids {
			if action == "collection-add" {
				if err = s.Tags.AddToCollection(ctx, c.Key, id); err == nil {
					err = s.DB.AddToCollection(ctx, c.Key, id)
				}
			} else if err = s.Tags.RemoveFromCollection(ctx, c.Key, id); err == nil {
				err = s.DB.RemoveFromCollection(ctx, c.Key, id)
			}
			if err != nil {
				fail(id, err)
			}
		}
	case "collection-new":
		title, err := cleanNewCollection(r.PostFormValue("title"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		detail = title
		// Plex makes a collection from its first item; the rest are added.
		key, err := s.Tags.CreateCollection(ctx, section, plexType(types[ids[0]]), title, ids[0])
		if err != nil {
			log.Printf("web: create collection: %v", err)
			http.Error(w, "Plex wouldn't make the collection: "+err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.DB.AddCollection(ctx, storage.PlexCollection{Key: key, SectionKey: section, Title: title, Subtype: types[ids[0]]}); err != nil {
			fail(ids[0], err)
		} else if err := s.DB.AddToCollection(ctx, key, ids[0]); err != nil {
			fail(ids[0], err)
		}
		for _, id := range ids[1:] {
			err := s.Tags.AddToCollection(ctx, key, id)
			if err == nil {
				err = s.DB.AddToCollection(ctx, key, id)
			}
			if err != nil {
				fail(id, err)
			}
		}
	default:
		http.Error(w, "pick what to do with the ticked items", http.StatusBadRequest)
		return
	}
	who := userFrom(r.Context()).Name
	s.DB.Audit(ctx, who, "tags: "+action, section, fmt.Sprintf("%q on %d items, %d failed", detail, len(ids), len(failures)))
	if len(failures) > 0 {
		log.Print("web: some tag changes failed; the audit log has the counts")
		http.Error(w, fmt.Sprintf("%d of %d failed, the rest were done:\n%s", len(failures), len(ids), strings.Join(failures[:min(len(failures), 10)], "\n")), http.StatusBadGateway)
		return
	}
	backToList(w, r, section)
}

// cleanNewCollection checks a new collection's name; a comma is fine in one.
func cleanNewCollection(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("type a name for the new collection")
	}
	if len([]rune(s)) > 100 || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", errors.New("that name is too long or holds a control character")
	}
	return s, nil
}

// itemLabel adds or removes one label on one item, from its card.
func (s *Server) itemLabel(w http.ResponseWriter, r *http.Request) {
	if s.tagsOff(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := chi.URLParam(r, "ratingKey")
	section, typ, found, err := s.DB.MediaInfo(r.Context(), key)
	if err != nil {
		log.Printf("web: media info: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	op := r.PostFormValue("op")
	if op != "add" && op != "remove" {
		http.Error(w, "add or remove", http.StatusBadRequest)
		return
	}
	name, err := cleanName(r.PostFormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if name, err = s.libraryLabel(ctx, section, name); err != nil {
		log.Printf("web: label names: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.changeLabel(ctx, section, typ, key, name, op == "add"); err != nil {
		log.Printf("web: label change: %v", err)
		http.Error(w, "Plex wouldn't change the label: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.DB.Audit(ctx, userFrom(r.Context()).Name, "tags: label "+op, key, name)
	backToCard(w, r, section, key)
}

// itemCollectionRemove takes one item out of one collection, from its card.
func (s *Server) itemCollectionRemove(w http.ResponseWriter, r *http.Request) {
	if s.tagsOff(w) {
		return
	}
	key, ck := chi.URLParam(r, "ratingKey"), chi.URLParam(r, "collectionKey")
	section, _, found, err := s.DB.MediaInfo(r.Context(), key)
	if err != nil {
		log.Printf("web: media info: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c, cfound, err := s.DB.Collection(r.Context(), ck)
	if err != nil {
		log.Printf("web: collection: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found || !cfound || c.SectionKey != section {
		http.NotFound(w, r)
		return
	}
	if c.Smart {
		http.Error(w, "a smart collection builds itself from a filter, so items can't be removed by hand", http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if err := s.Tags.RemoveFromCollection(ctx, ck, key); err != nil {
		log.Printf("web: collection remove: %v", err)
		http.Error(w, "Plex wouldn't take it out: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.DB.RemoveFromCollection(ctx, ck, key); err != nil {
		log.Printf("web: collection mirror: %v", err)
	}
	s.DB.Audit(ctx, userFrom(r.Context()).Name, "tags: collection remove", key, c.Title)
	backToCard(w, r, section, key)
}

// labels lists every label in every library with how many items carry it.
func (s *Server) labels(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	rows, err := s.DB.Labels(r.Context())
	if err != nil {
		log.Printf("web: labels: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "labels", page{User: &u, LabelRows: rows})
}

// removeLabelEverywhere takes a label off every item in a library that carries it.
func (s *Server) removeLabelEverywhere(w http.ResponseWriter, r *http.Request) {
	if s.tagsOff(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	section, name := r.PostFormValue("section"), r.PostFormValue("label")
	ctx := context.WithoutCancel(r.Context())
	keys, err := s.DB.ItemsWithLabel(ctx, section, name)
	if err != nil {
		log.Printf("web: items with label: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(keys) == 0 {
		http.Error(w, "no item in that library carries that label", http.StatusNotFound)
		return
	}
	types, err := s.DB.MediaTypes(ctx, section, keys)
	if err != nil {
		log.Printf("web: media types: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var failures []string
	for _, k := range keys {
		if err := s.changeLabel(ctx, section, types[k], k, name, false); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", k, err))
		}
	}
	s.DB.Audit(ctx, userFrom(r.Context()).Name, "tags: label removed everywhere", section, fmt.Sprintf("%q from %d items, %d failed", name, len(keys), len(failures)))
	if len(failures) > 0 {
		log.Print("web: removing a label from every item failed for some; the audit log has the counts")
		http.Error(w, fmt.Sprintf("%d of %d failed, the rest were done:\n%s", len(failures), len(keys), strings.Join(failures[:min(len(failures), 10)], "\n")), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/labels", http.StatusSeeOther)
}

// collections lists every collection in every library.
func (s *Server) collections(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	rows, err := s.DB.AllCollections(r.Context())
	if err != nil {
		log.Printf("web: collections: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "collections", page{User: &u, CollectionRows: rows})
}

// deleteCollection deletes a collection and only the collection: the items stay in the library.
func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	if s.tagsOff(w) {
		return
	}
	key := chi.URLParam(r, "key")
	c, found, err := s.DB.Collection(r.Context(), key)
	if err != nil {
		log.Printf("web: collection: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if err := s.Tags.DeleteCollection(ctx, key); err != nil {
		log.Printf("web: delete collection: %v", err)
		http.Error(w, "Plex wouldn't delete the collection: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.DB.DeleteCollection(ctx, key); err != nil {
		log.Printf("web: collection mirror: %v", err)
	}
	s.DB.Audit(ctx, userFrom(r.Context()).Name, "tags: collection deleted", key, fmt.Sprintf("%s (%d items kept)", c.Title, c.ItemCount))
	http.Redirect(w, r, "/collections", http.StatusSeeOther)
}
