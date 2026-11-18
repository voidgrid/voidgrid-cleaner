package storage

import (
	"context"
	"database/sql"
	"strings"
)

// Labels and collections as Plex has them, copied at sync. Plex is the source: the app writes to
// Plex first and then mirrors the change here, so the library reads them without asking Plex.

// SectionLabel is a label name in a library.
type SectionLabel struct{ SectionKey, Title string }

// ItemLabel is a label on an item.
type ItemLabel struct{ RatingKey, Label string }

// PlexCollection is a collection in a library; ItemCount is Plex's own count.
type PlexCollection struct {
	Key, SectionKey, Title, Subtype string
	SectionTitle                    string // filled when listing every library's collections
	Smart                           bool
	ItemCount                       int
}

// CollectionMember is an item in a collection.
type CollectionMember struct{ RatingKey, CollectionKey string }

// TagSnapshot is everything one sync read.
type TagSnapshot struct {
	Labels      []SectionLabel
	ItemLabels  []ItemLabel
	Collections []PlexCollection
	Members     []CollectionMember
}

// ReplaceTags makes the stored labels and collections exactly the snapshot.
func (db *DB) ReplaceTags(ctx context.Context, s TagSnapshot) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM plex_labels`, `DELETE FROM item_labels`, `DELETE FROM plex_collections`, `DELETE FROM item_collections`} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		for _, l := range s.Labels {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO plex_labels (section_key, title) VALUES (?, ?)`, l.SectionKey, l.Title); err != nil {
				return err
			}
		}
		for _, l := range s.ItemLabels {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_labels (rating_key, label) VALUES (?, ?)`, l.RatingKey, l.Label); err != nil {
				return err
			}
		}
		for _, c := range s.Collections {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO plex_collections (key, section_key, title, subtype, smart, item_count) VALUES (?, ?, ?, ?, ?, ?)`,
				c.Key, c.SectionKey, c.Title, c.Subtype, c.Smart, c.ItemCount); err != nil {
				return err
			}
		}
		for _, m := range s.Members {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_collections (rating_key, collection_key) VALUES (?, ?)`, m.RatingKey, m.CollectionKey); err != nil {
				return err
			}
		}
		return nil
	})
}

// LabelRow is one label in one library, with how many items carry it.
type LabelRow struct {
	SectionKey, SectionTitle, Title string
	Items                           int
}

// Labels lists every label in every library that isn't ignored, with item counts.
func (db *DB) Labels(ctx context.Context) ([]LabelRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT s.section_key, s.title, pl.title,
			(SELECT count(*) FROM item_labels il JOIN media m ON m.rating_key = il.rating_key
				WHERE m.section_key = pl.section_key AND il.label = pl.title)
		FROM plex_labels pl JOIN sections s ON s.section_key = pl.section_key AND s.ignored = 0
		ORDER BY s.position, pl.title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LabelRow
	for rows.Next() {
		var r LabelRow
		if err := rows.Scan(&r.SectionKey, &r.SectionTitle, &r.Title, &r.Items); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LabelNames lists a library's label names, as Plex spells them.
func (db *DB) LabelNames(ctx context.Context, sectionKey string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT title FROM plex_labels WHERE section_key = ? ORDER BY title COLLATE NOCASE`, sectionKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ItemsWithLabel lists the rating keys in a library that carry a label.
func (db *DB) ItemsWithLabel(ctx context.Context, sectionKey, label string) ([]string, error) {
	return db.keys(ctx, `SELECT il.rating_key FROM item_labels il JOIN media m ON m.rating_key = il.rating_key
		WHERE m.section_key = ? AND il.label = ? ORDER BY m.title COLLATE NOCASE`, sectionKey, label)
}

func (db *DB) keys(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// MediaTypes returns the type ("movie" or "show") of each of keys that is in the library,
// so a request naming an item from another library, or none, is told apart.
func (db *DB) MediaTypes(ctx context.Context, sectionKey string, keys []string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	args := []any{sectionKey}
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := db.QueryContext(ctx, `SELECT rating_key, type FROM media WHERE section_key = ? AND rating_key IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k, t string
		if err := rows.Scan(&k, &t); err != nil {
			return nil, err
		}
		out[k] = t
	}
	return out, rows.Err()
}

// AddItemLabel records that Plex now has a label on an item, and the name in the library.
func (db *DB) AddItemLabel(ctx context.Context, sectionKey, ratingKey, label string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO plex_labels (section_key, title) VALUES (?, ?)`, sectionKey, label); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_labels (rating_key, label) VALUES (?, ?)`, ratingKey, label)
		return err
	})
}

// RemoveItemLabel records that an item lost a label. The name stays in the library's list, as in Plex.
func (db *DB) RemoveItemLabel(ctx context.Context, ratingKey, label string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM item_labels WHERE rating_key = ? AND label = ?`, ratingKey, label)
	return err
}

// CollectionsIn lists a library's collections by title.
func (db *DB) CollectionsIn(ctx context.Context, sectionKey string) ([]PlexCollection, error) {
	return db.collections(ctx, ` WHERE c.section_key = ?`, sectionKey)
}

// AllCollections lists every collection in every library that isn't ignored, by library and title.
func (db *DB) AllCollections(ctx context.Context) ([]PlexCollection, error) {
	return db.collections(ctx, ` WHERE s.ignored = 0`)
}

func (db *DB) collections(ctx context.Context, from string, args ...any) ([]PlexCollection, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.key, c.section_key, c.title, c.subtype, c.smart, c.item_count, coalesce(s.title, '')
		FROM plex_collections c LEFT JOIN sections s ON s.section_key = c.section_key`+from+
		` ORDER BY coalesce(s.position, 0), c.title COLLATE NOCASE`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PlexCollection
	for rows.Next() {
		var c PlexCollection
		if err := rows.Scan(&c.Key, &c.SectionKey, &c.Title, &c.Subtype, &c.Smart, &c.ItemCount, &c.SectionTitle); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Collection finds one collection; found is false if there is none.
func (db *DB) Collection(ctx context.Context, key string) (c PlexCollection, found bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT key, section_key, title, subtype, smart, item_count FROM plex_collections WHERE key = ?`, key).
		Scan(&c.Key, &c.SectionKey, &c.Title, &c.Subtype, &c.Smart, &c.ItemCount)
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	return c, err == nil, err
}

// ItemsInCollection lists the rating keys of library items in a collection.
func (db *DB) ItemsInCollection(ctx context.Context, key string) ([]string, error) {
	return db.keys(ctx, `SELECT ic.rating_key FROM item_collections ic JOIN media m ON m.rating_key = ic.rating_key
		WHERE ic.collection_key = ? ORDER BY m.title COLLATE NOCASE`, key)
}

// AddCollection records a collection Plex just made.
func (db *DB) AddCollection(ctx context.Context, c PlexCollection) error {
	_, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO plex_collections (key, section_key, title, subtype, smart, item_count) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Key, c.SectionKey, c.Title, c.Subtype, c.Smart, c.ItemCount)
	return err
}

// AddToCollection records an item joining a collection.
func (db *DB) AddToCollection(ctx context.Context, collectionKey, ratingKey string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_collections (rating_key, collection_key) VALUES (?, ?)`, ratingKey, collectionKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE plex_collections SET item_count = item_count + 1 WHERE key = ?`, collectionKey)
		}
		return err
	})
}

// RemoveFromCollection records an item leaving a collection.
func (db *DB) RemoveFromCollection(ctx context.Context, collectionKey, ratingKey string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM item_collections WHERE rating_key = ? AND collection_key = ?`, ratingKey, collectionKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			_, err = tx.ExecContext(ctx, `UPDATE plex_collections SET item_count = max(item_count - 1, 0) WHERE key = ?`, collectionKey)
		}
		return err
	})
}

// DeleteCollection records a collection gone from Plex, its members with it.
func (db *DB) DeleteCollection(ctx context.Context, key string) error {
	return db.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_collections WHERE collection_key = ?`, key); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM plex_collections WHERE key = ?`, key)
		return err
	})
}
