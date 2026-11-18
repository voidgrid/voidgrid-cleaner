package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Labels (Plex's "tags" on an item) and collections. Writes go through the same
// calls Plex's own edit dialogs make: PUT /library/sections/{id}/all for labels,
// /library/collections for collections.

// Media types the edit calls take.
const (
	TypeMovie = 1
	TypeShow  = 2
)

// Label is a label name Plex knows in a library. Plex keeps the name after the
// last item loses it, so a label can have no items.
type Label struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// Labels lists the label names in a library.
func (s *Server) Labels(ctx context.Context, sectionKey string) ([]Label, error) {
	var r struct {
		MediaContainer struct {
			Directory []Label `json:"Directory"`
		} `json:"MediaContainer"`
	}
	err := s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/label", &r)
	return r.MediaContainer.Directory, err
}

// ratingKeys lists the items a Plex listing returned.
type listing struct {
	MediaContainer struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

func (l listing) keys() []string {
	out := make([]string, 0, len(l.MediaContainer.Metadata))
	for _, m := range l.MediaContainer.Metadata {
		out = append(out, m.RatingKey)
	}
	return out
}

// LabelItems lists the rating keys of a library's items that carry a label.
func (s *Server) LabelItems(ctx context.Context, sectionKey, labelKey string) ([]string, error) {
	var l listing
	err := s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/all?label="+url.QueryEscape(labelKey), &l)
	return l.keys(), err
}

// queryValue escapes a name for a query string the way Plex reads it: spaces as %20.
func queryValue(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }

// EditLabels adds and removes labels on one item and leaves its other labels alone.
// typ is TypeMovie or TypeShow. A name can't hold a comma, which Plex reads as a list.
func (s *Server) EditLabels(ctx context.Context, sectionKey string, typ int, ratingKey string, add, remove []string) error {
	var q strings.Builder
	fmt.Fprintf(&q, "/library/sections/%s/all?type=%d&id=%s", url.PathEscape(sectionKey), typ, queryValue(ratingKey))
	for i, name := range add {
		if strings.Contains(name, ",") {
			return fmt.Errorf("plex: a label name can't hold a comma: %q", name)
		}
		fmt.Fprintf(&q, "&label[%d].tag.tag=%s", i, queryValue(name))
	}
	if len(remove) > 0 {
		for _, name := range remove {
			if strings.Contains(name, ",") {
				return fmt.Errorf("plex: a label name can't hold a comma: %q", name)
			}
		}
		parts := make([]string, len(remove))
		for i, name := range remove {
			parts[i] = queryValue(name)
		}
		q.WriteString("&label[].tag.tag-=" + strings.Join(parts, ","))
	}
	q.WriteString("&label.locked=1") // so a metadata refresh doesn't undo it
	return s.do(ctx, http.MethodPut, q.String(), nil)
}

// Collection is a collection in a library. Subtype says what it holds: "movie",
// "show" or "episode".
type Collection struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Subtype   string `json:"subtype"`
	Count     int    `json:"childCount"`
	Smart     bool   `json:"-"`
}

// Collections lists a library's collections.
func (s *Server) Collections(ctx context.Context, sectionKey string) ([]Collection, error) {
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				Collection
				// Smart collections build themselves from a filter; Plex sends true or "1".
				Smart json.RawMessage `json:"smart"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := s.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/collections", &r); err != nil {
		return nil, err
	}
	out := make([]Collection, 0, len(r.MediaContainer.Metadata))
	for _, m := range r.MediaContainer.Metadata {
		c := m.Collection
		c.Smart = string(m.Smart) == "true" || string(m.Smart) == `"1"` || string(m.Smart) == "1"
		out = append(out, c)
	}
	return out, nil
}

// CollectionItems lists the rating keys in a collection.
func (s *Server) CollectionItems(ctx context.Context, collectionKey string) ([]string, error) {
	var l listing
	err := s.do(ctx, http.MethodGet, "/library/collections/"+url.PathEscape(collectionKey)+"/children", &l)
	return l.keys(), err
}

// DeleteCollection deletes a collection and only the collection: its items stay in the library.
func (s *Server) DeleteCollection(ctx context.Context, collectionKey string) error {
	return s.do(ctx, http.MethodDelete, "/library/collections/"+url.PathEscape(collectionKey), nil)
}

// RemoveFromCollection takes an item out of a collection, leaving it in the library.
func (s *Server) RemoveFromCollection(ctx context.Context, collectionKey, ratingKey string) error {
	return s.do(ctx, http.MethodDelete, "/library/collections/"+url.PathEscape(collectionKey)+"/items/"+url.PathEscape(ratingKey), nil)
}

// machineIdentifier is the server's ID, which collection calls name items by.
func (s *Server) machineIdentifier(ctx context.Context) (string, error) {
	s.machineMu.Lock()
	defer s.machineMu.Unlock()
	if s.machineID != "" {
		return s.machineID, nil
	}
	var r struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
		} `json:"MediaContainer"`
	}
	if err := s.do(ctx, http.MethodGet, "/identity", &r); err != nil {
		return "", err
	}
	if r.MediaContainer.MachineIdentifier == "" {
		return "", fmt.Errorf("plex: the server gave no machine identifier")
	}
	s.machineID = r.MediaContainer.MachineIdentifier
	return s.machineID, nil
}

// itemURI names a library item the way collection calls want it.
func (s *Server) itemURI(ctx context.Context, ratingKey string) (string, error) {
	id, err := s.machineIdentifier(ctx)
	if err != nil {
		return "", err
	}
	return queryValue("server://" + id + "/com.plexapp.plugins.library/library/metadata/" + ratingKey), nil
}

// AddToCollection puts an item in a collection.
func (s *Server) AddToCollection(ctx context.Context, collectionKey, ratingKey string) error {
	uri, err := s.itemURI(ctx, ratingKey)
	if err != nil {
		return err
	}
	return s.do(ctx, http.MethodPut, "/library/collections/"+url.PathEscape(collectionKey)+"/items?uri="+uri, nil)
}

// CreateCollection makes a collection holding one item, which Plex requires, and returns its key.
// typ is TypeMovie or TypeShow.
func (s *Server) CreateCollection(ctx context.Context, sectionKey string, typ int, title, ratingKey string) (string, error) {
	uri, err := s.itemURI(ctx, ratingKey)
	if err != nil {
		return "", err
	}
	var r struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	path := "/library/collections?type=" + strconv.Itoa(typ) + "&title=" + queryValue(title) + "&smart=0&sectionId=" + url.QueryEscape(sectionKey) + "&uri=" + uri
	if err := s.do(ctx, http.MethodPost, path, &r); err != nil {
		return "", err
	}
	if len(r.MediaContainer.Metadata) == 0 || r.MediaContainer.Metadata[0].RatingKey == "" {
		return "", fmt.Errorf("plex: the server didn't say which collection it made")
	}
	return r.MediaContainer.Metadata[0].RatingKey, nil
}
