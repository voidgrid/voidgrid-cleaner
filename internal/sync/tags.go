package sync

import (
	"context"
	"fmt"

	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
)

// tags copies Plex's labels and collections for every library that isn't ignored. Plex's
// library listing carries neither, so each label and each collection is asked for its items.
// Any failure leaves the last good copy: half a copy would read as labels and collections gone.
func (s *Syncer) tags(ctx context.Context) (int, error) {
	secs, err := s.DB.Sections(ctx)
	if err != nil {
		return 0, err
	}
	var snap storage.TagSnapshot
	for _, sec := range secs {
		if sec.Ignored || (sec.Type != "movie" && sec.Type != "show") {
			continue
		}
		labels, err := s.Plex.Labels(ctx, sec.Key)
		if err != nil {
			return 0, fmt.Errorf("labels in %s: %w", sec.Title, err)
		}
		for _, l := range labels {
			snap.Labels = append(snap.Labels, storage.SectionLabel{SectionKey: sec.Key, Title: l.Title})
			keys, err := s.Plex.LabelItems(ctx, sec.Key, l.Key)
			if err != nil {
				return 0, fmt.Errorf("items labelled %s in %s: %w", l.Title, sec.Title, err)
			}
			for _, k := range keys {
				snap.ItemLabels = append(snap.ItemLabels, storage.ItemLabel{RatingKey: k, Label: l.Title})
			}
		}
		cols, err := s.Plex.Collections(ctx, sec.Key)
		if err != nil {
			return 0, fmt.Errorf("collections in %s: %w", sec.Title, err)
		}
		for _, c := range cols {
			snap.Collections = append(snap.Collections, storage.PlexCollection{
				Key: c.RatingKey, SectionKey: sec.Key, Title: c.Title, Subtype: c.Subtype, Smart: c.Smart, ItemCount: c.Count,
			})
			keys, err := s.Plex.CollectionItems(ctx, c.RatingKey)
			if err != nil {
				return 0, fmt.Errorf("items in collection %s: %w", c.Title, err)
			}
			for _, k := range keys {
				snap.Members = append(snap.Members, storage.CollectionMember{RatingKey: k, CollectionKey: c.RatingKey})
			}
		}
	}
	return len(snap.ItemLabels) + len(snap.Members), s.DB.ReplaceTags(ctx, snap)
}
