// Package web serves the server-rendered, terminal-themed UI.
package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/discord"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
	"github.com/voidgrid/voidgrid-cleaner/internal/doctor"
	"github.com/voidgrid/voidgrid-cleaner/internal/storage"
	webassets "github.com/voidgrid/voidgrid-cleaner/web"
)

const (
	sessionCookie = "vgc_session"
	pinCookie     = "vgc_plex_pin"
	sessionTTL    = 30 * 24 * time.Hour
)

// Server holds what the handlers need.
type Server struct {
	DryRun bool // deletions only log what they would do
	// DiscordKey and DiscordChannel accept Keep presses; nil turns them off.
	DiscordKey     ed25519.PublicKey
	DiscordChannel string
	DB             *storage.DB
	Seerr          *seerr.Client
	PlexTV         *plex.TV
	// PinWait is how long the Plex callback waits for plex.tv to report
	// the pin approved (it can lag the redirect slightly).
	PinWait time.Duration
	// SyncNow starts a sync in the background.
	SyncNow func()
	// DoctorNow starts a doctor run in the background.
	DoctorNow func()
	// Previews says what doctor fixes would do.
	Previews Previewer
	// Posters resizes Plex artwork; PosterCache holds the results on disk.
	Posters     PosterSource
	PosterCache *os.Root

	limiter limiter
}

type page struct {
	User      *storage.User
	Error     string
	Email     string
	Sync      *syncPage
	Library   *libraryPage
	Libraries []storage.Section
	Accounts  []storage.PlexAccount
	Discord   []storage.DiscordUser
	Users     []storage.User
	Status    *statusPage
	Settings  *settingsPage
	Ephemeral []storage.EphemeralShow
	Doctor    *doctorPage
	Log       *logPage
}

// Previewer builds fix previews.
type Previewer interface {
	Preview(context.Context, storage.Finding) (doctor.Preview, error)
}

type doctorPage struct {
	Groups         []findingGroup
	Dismissed      bool
	DismissedCount int
	Fixes          []storage.FixJob
	Previews       []doctor.Preview
}

type findingGroup struct {
	Label    string
	Fixable  bool
	Findings []storage.Finding
}

type logPage struct {
	Entries []storage.AuditEntry
	Search  string
	More    string
}

type settingsPage struct {
	Window        storage.DeleteWindow
	Zone          string
	SpaceLimit    string
	DefaultSpace  int
	DefaultWin    storage.DeleteWindow
	AdminDiscord  string
	LogExpiry     string
	LogLevel      string
	DigestTime    string
	DefaultDigest string
	DefaultLevel  string
	DefaultExpiry int
	Paths         []storage.LibraryPath
	Saved         bool
}

type statusPage struct {
	DryRun bool
	Active []storage.Deletion
	Recent []storage.Deletion
	// Waiting lists ephemeral shows with episodes someone on them hasn't seen.
	Waiting []storage.EpisodeWait
}

// PosterSource returns a Plex thumb resized to width x height as JPEG.
type PosterSource interface {
	Poster(ctx context.Context, thumb string, width, height int) ([]byte, error)
}

type libraryPage struct {
	Sections []storage.Section
	Current  storage.Section
	Items    []storage.LibraryItem
	Filter   storage.LibraryFilter
	Total    int
	More     string // URL of the next items, "" at the end
}

// libraryPageSize is how many cards a library page starts with: four rows.
const libraryPageSize = 32

type syncPage struct {
	Statuses  []storage.SyncStatus
	Counts    map[string]int
	Unmatched []storage.Unmatched
	Started   bool
}

var pages = map[string]*template.Template{}

func init() {
	funcs := template.FuncMap{
		"when": func(unix int64) string {
			if unix == 0 {
				return "never"
			}
			return time.Unix(unix, 0).Format("2006-01-02 15:04 MST")
		},
		"day": func(unix int64) string {
			if unix == 0 {
				return "never"
			}
			return time.Unix(unix, 0).UTC().Format("2006-01-02")
		},
		"statusName": func(s string) string { return strings.ReplaceAll(s, "_", "-") },
		// Ephemeral is per episode, so only shows offer it.
		"statuses": func(mediaType string) []string {
			out := []string{storage.StatusNormal, storage.StatusKeep, storage.StatusSpace}
			if mediaType == "show" {
				out = append(out, storage.StatusEphemeral)
			}
			return out
		},
		"fixable": doctor.Fixable,
		"gb":      func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 1, 64) + " GB" },
	}
	for _, name := range []string{"login", "home", "sync", "library", "libraries", "users", "settings", "ephemeral", "doctor", "doctor-preview", "log"} {
		pages[name] = template.Must(template.New("layout").Funcs(funcs).ParseFS(webassets.FS, "templates/layout.html", "templates/"+name+".html"))
	}
}

func render(w http.ResponseWriter, status int, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := pages[name].ExecuteTemplate(w, "layout", p); err != nil {
		log.Printf("web: render %s: %v", name, err)
	}
}

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewMux()
	r.Use(securityHeaders, sameOrigin)
	r.Handle("/static/*", http.FileServerFS(webassets.FS))
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	r.Get("/login", func(w http.ResponseWriter, r *http.Request) { render(w, http.StatusOK, "login", page{}) })
	r.Post("/login/local", s.loginLocal)
	r.Get("/login/plex", s.loginPlex)
	r.Get("/login/plex/callback", s.loginPlexCallback)
	r.Post("/logout", s.logout)
	// Signed by Discord, not a browser session; see discordInteraction.
	r.Post("/discord/interactions", s.discordInteraction)
	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/", s.status)
		r.Get("/changes", s.changes)
		r.Get("/library", s.library)
		r.Get("/library/{key}", s.library)
		r.Get("/poster/{ratingKey}", s.poster)
		r.Get("/ephemeral", s.ephemeral)
		r.Post("/ephemeral/{ratingKey}/join", s.joinEphemeral)
		r.Group(func(r chi.Router) {
			r.Use(requireAdmin)
			r.Get("/sync", s.syncStatus)
			r.Get("/libraries", s.libraries)
			r.Get("/users", s.users)
			r.Get("/settings", s.settings)
			r.Get("/doctor", s.doctor)
			r.Post("/doctor/check", s.doctorCheck)
			r.Get("/doctor/preview", s.doctorPreview)
			r.Post("/doctor/apply", s.doctorApply)
			r.Post("/doctor/{id}/dismiss", s.setFinding("dismissed"))
			r.Post("/doctor/{id}/open", s.setFinding("open"))
			r.Get("/log", s.auditLog)
			r.Post("/settings", s.saveSettings)
			r.Post("/users/plex/{id}", s.setPlexAccount)
			r.Post("/users/discord/{id}", s.setDiscordUser)
			// shortcut: admins only until the request flow lets users ask for changes.
			r.Post("/library/item/{ratingKey}/delete", s.scheduleDeletion)
			r.Post("/library/item/{ratingKey}/cancel", s.changeDeletion("cancel deletion", s.DB.CancelDeletion))
			r.Post("/library/item/{ratingKey}/postpone", s.changeDeletion("postpone deletion", s.DB.PostponeDeletion))
			r.Post("/library/item/{ratingKey}/{status}", s.setStatus)
			r.Post("/libraries/{key}/ignore", s.setIgnored(true))
			r.Post("/libraries/{key}/unignore", s.setIgnored(false))
			r.Post("/sync", func(w http.ResponseWriter, r *http.Request) {
				s.SyncNow()
				http.Redirect(w, r, "/sync?started=1", http.StatusSeeOther)
			})
		})
	})
	return r
}

func (s *Server) syncStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	st, err := s.DB.SyncStatuses(r.Context())
	if err != nil {
		log.Printf("web: sync statuses: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	counts, items, err := s.DB.SyncReport(r.Context(), 50)
	if err != nil {
		log.Printf("web: sync report: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "sync", page{User: &u, Sync: &syncPage{
		Statuses: st, Counts: counts, Unmatched: items, Started: r.URL.Query().Get("started") == "1",
	}})
}

// libraries lists every video library, for ignoring and unignoring.
func (s *Server) libraries(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	secs, err := s.DB.Sections(r.Context())
	if err != nil {
		log.Printf("web: sections: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "libraries", page{User: &u, Libraries: secs})
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	accounts, err := s.DB.PlexAccounts(r.Context())
	if err != nil {
		log.Printf("web: plex accounts: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	users, err := s.DB.Users(r.Context())
	if err != nil {
		log.Printf("web: users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	discordUsers, err := s.DB.DiscordUsers(r.Context())
	if err != nil {
		log.Printf("web: discord users: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "users", page{User: &u, Accounts: accounts, Users: users, Discord: discordUsers})
}

func (s *Server) setDiscordUser(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var seerrID int64
	if v := r.PostFormValue("seerr_user"); v != "" {
		var err error
		if seerrID, err = strconv.ParseInt(v, 10, 64); err != nil || seerrID == 0 {
			http.Error(w, "bad user", http.StatusBadRequest)
			return
		}
	}
	found, err := s.DB.SetDiscordUser(r.Context(), chi.URLParam(r, "id"), seerrID)
	if err != nil {
		log.Printf("web: set discord user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "link discord user", chi.URLParam(r, "id"), "seerr user "+strconv.FormatInt(seerrID, 10))
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

// discordInteraction handles Discord's calls. The signature check comes
// before anything reads the body, and the only action is Keep, which can stop
// a deletion but never start one.
func (s *Server) discordInteraction(w http.ResponseWriter, r *http.Request) {
	if s.DiscordKey == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || !discord.Verify(s.DiscordKey, r.Header.Get("X-Signature-Ed25519"), r.Header.Get("X-Signature-Timestamp"), body, time.Now()) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	var in struct {
		Type      int    `json:"type"`
		ChannelID string `json:"channel_id"`
		Data      struct {
			CustomID string `json:"custom_id"`
		} `json:"data"`
		Member struct {
			Nick string `json:"nick"`
			User struct {
				ID         string `json:"id"`
				Username   string `json:"username"`
				GlobalName string `json:"global_name"`
			} `json:"user"`
		} `json:"member"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("web: discord reply: %v", err)
		}
	}
	private := func(text string) { // only the presser sees it
		reply(map[string]any{"type": 4, "data": map[string]any{"content": text, "flags": 64}})
	}
	const ping, component = 1, 3
	if in.Type == ping {
		reply(map[string]any{"type": 1})
		return
	}
	id, isKeep := discord.ParseKeep(in.Data.CustomID)
	show, isJoin := discord.ParseJoin(in.Data.CustomID)
	if in.Type != component || (!isKeep && !isJoin) || in.ChannelID != s.DiscordChannel || in.Member.User.ID == "" {
		private("That can't be done here.")
		return
	}
	name := in.Member.Nick
	for _, n := range []string{in.Member.User.GlobalName, in.Member.User.Username} {
		if name == "" {
			name = n
		}
	}
	if isJoin {
		title, result, err := s.DB.JoinFromDiscord(r.Context(), show, in.Member.User.ID, name)
		if err == nil && result != storage.JoinGone {
			s.DB.Audit(r.Context(), "discord: "+name, "add me", title, "")
		}
		switch {
		case err != nil:
			log.Printf("web: discord join: %v", err)
			private("Something went wrong, so you weren't added. Tell an admin.")
		case result == storage.JoinAdded:
			private("You're on **" + title + "**: episodes now wait until you've watched them too.")
		case result == storage.JoinPending:
			private("Your Discord account isn't linked yet, so an admin has been asked. You'll be added to **" + title + "** when that's done.")
		default:
			private("That show isn't ephemeral any more.")
		}
		return
	}
	title, kept, err := s.DB.KeepFromDiscord(r.Context(), id, in.Member.User.ID, name)
	if err != nil {
		log.Printf("web: discord keep: %v", err)
		private("Something went wrong, so it wasn't kept. Tell an admin.")
		return
	}
	if !kept {
		private("Too late: that deletion isn't waiting any more.")
		return
	}
	s.DB.Audit(r.Context(), "discord: "+name, "keep", title, "")
	// Update the message, dropping the button. Mentions stay off for the name too.
	reply(map[string]any{"type": 7, "data": map[string]any{
		"content": "**" + title + "** was kept by " + name + ".", "components": []any{},
		"allowed_mentions": map[string]any{"parse": []string{}},
	}})
}

func (s *Server) setPlexAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	// An empty choice unlinks the account.
	var seerrID int64
	if v := r.PostFormValue("seerr_user"); v != "" {
		if seerrID, err = strconv.ParseInt(v, 10, 64); err != nil || seerrID == 0 {
			http.Error(w, "bad user", http.StatusBadRequest)
			return
		}
	}
	found, err := s.DB.SetPlexAccount(r.Context(), id, seerrID, userFrom(r.Context()).SeerrID)
	if err != nil {
		log.Printf("web: set plex account: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "link plex account", strconv.FormatInt(id, 10), "seerr user "+strconv.FormatInt(seerrID, 10))
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) setIgnored(ignored bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		found, err := s.DB.SetSectionIgnored(r.Context(), chi.URLParam(r, "key"), ignored)
		if err != nil {
			log.Printf("web: set ignored: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "set library ignored", chi.URLParam(r, "key"), strconv.FormatBool(ignored))
		if !ignored {
			s.SyncNow() // its items were dropped when it was ignored
		}
		http.Redirect(w, r, "/libraries", http.StatusSeeOther)
	}
}

// library shows one tab per visible Plex library. /library opens the first.
func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	secs, err := s.DB.VisibleSections(r.Context(), u)
	if err != nil {
		log.Printf("web: sections: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	key := chi.URLParam(r, "key")
	if key == "" {
		if len(secs) == 0 {
			render(w, http.StatusOK, "library", page{User: &u, Library: &libraryPage{}})
			return
		}
		http.Redirect(w, r, "/library/"+url.PathEscape(secs[0].Key), http.StatusSeeOther)
		return
	}
	i := slices.IndexFunc(secs, func(sec storage.Section) bool { return sec.Key == key })
	if i < 0 {
		// Unknown, or nothing in it the user requested. A full page, not a bare
		// error, so the menu and the user's own tabs still work.
		render(w, http.StatusForbidden, "library", page{
			User: &u, Library: &libraryPage{Sections: secs},
			Error: "I'm sorry Dave, I can't do that",
		})
		return
	}
	v := r.URL.Query()
	days := func(name string) int {
		n, err := strconv.Atoi(v.Get(name))
		if err != nil || n < 0 {
			return 0
		}
		return min(n, 36500) // keeps the date maths sane
	}
	f := storage.LibraryFilter{
		Search: strings.TrimSpace(v.Get("q")), Sort: v.Get("sort"), Status: v.Get("status"),
		AddedDays: days("added"), UnwatchedDays: days("unwatched"), RequesterWatched: v.Get("requester") == "1",
		Limit: libraryPageSize,
	}
	// The page script asks for everything it has loaded when it refreshes.
	if n, err := strconv.Atoi(v.Get("limit")); err == nil && n > 0 {
		f.Limit = min(n, 10000)
	}
	if n, err := strconv.Atoi(v.Get("offset")); err == nil && n > 0 {
		f.Offset = n
	}
	items, err := s.DB.LibraryItems(r.Context(), u, key, f)
	if err != nil {
		log.Printf("web: library items: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	lib := &libraryPage{Sections: secs, Current: secs[i], Items: items, Filter: f}
	if len(items) > 0 {
		lib.Total = items[0].Total
	}
	if next := f.Offset + len(items); next < lib.Total {
		v.Del("limit")
		v.Set("offset", strconv.Itoa(next))
		lib.More = "/library/" + url.PathEscape(key) + "?" + v.Encode()
	}
	render(w, http.StatusOK, "library", page{User: &u, Library: lib})
}

// changes tells open pages whether anything they show has changed.
func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	v, err := s.DB.ChangeVersion(r.Context())
	if err != nil {
		log.Printf("web: change version: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `{"version":%d}`, v)
}

// setStatus records a status for an item and returns to the same view.
func (s *Server) setStatus(w http.ResponseWriter, r *http.Request) {
	key, status := chi.URLParam(r, "ratingKey"), chi.URLParam(r, "status")
	if !storage.ValidStatus(status) {
		http.Error(w, "no such status", http.StatusBadRequest)
		return
	}
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
	if status == storage.StatusEphemeral && typ != "show" {
		http.Error(w, "only shows can be ephemeral", http.StatusBadRequest)
		return
	}
	if err := s.DB.SetItemStatus(r.Context(), key, status, userFrom(r.Context()).SeerrID); err != nil {
		log.Printf("web: set status: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "set status", key, status)
	backToCard(w, r, section, key)
}

// backToCard returns to where the action came from: the status screen, or
// the item's card with the search and filters kept.
func backToCard(w http.ResponseWriter, r *http.Request, section, key string) {
	dest := url.URL{Path: "/library/" + section, Fragment: "card-" + key}
	// Only the path check and query are taken from the referrer.
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host {
		if ref.Path == "/" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		dest.RawQuery = ref.Query().Encode()
	}
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
}

// ephemeral lists ephemeral shows so anyone can add themselves, including
// people who can't see a show in their library because they didn't request it.
func (s *Server) ephemeral(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	shows, err := s.DB.EphemeralShows(r.Context(), u)
	if err != nil {
		log.Printf("web: ephemeral: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "ephemeral", page{User: &u, Ephemeral: shows})
}

func (s *Server) joinEphemeral(w http.ResponseWriter, r *http.Request) {
	// False means it stopped being ephemeral; the list shows that.
	key := chi.URLParam(r, "ratingKey")
	added, err := s.DB.AddWatcher(r.Context(), key, userFrom(r.Context()).SeerrID)
	if err != nil {
		log.Printf("web: join ephemeral: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if added {
		s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "add me", key, "")
	}
	http.Redirect(w, r, "/ephemeral", http.StatusSeeOther)
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	dismissed := r.URL.Query().Get("dismissed") == "1"
	state := "open"
	if dismissed {
		state = "dismissed"
	}
	fs, err := s.DB.Findings(r.Context(), state)
	if err != nil {
		log.Printf("web: findings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	other, err := s.DB.Findings(r.Context(), "dismissed")
	if err != nil {
		log.Printf("web: findings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	fixes, err := s.DB.RecentFixes(r.Context(), recentFixes)
	if err != nil {
		log.Printf("web: fixes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p := &doctorPage{Dismissed: dismissed, DismissedCount: len(other), Fixes: fixes}
	for _, l := range doctor.Labels {
		g := findingGroup{Label: l.Label}
		for _, f := range fs {
			if f.Kind == l.Kind {
				g.Findings = append(g.Findings, f)
				g.Fixable = g.Fixable || doctor.Fixable(f.Kind, f.Subject)
			}
		}
		if len(g.Findings) > 0 {
			p.Groups = append(p.Groups, g)
		}
	}
	render(w, http.StatusOK, "doctor", page{User: &u, Doctor: p})
}

// recentFixes is how many finished fixes the doctor page shows.
const recentFixes = 15

// doctorPreview shows what the ticked fixes would do. It changes nothing.
func (s *Server) doctorPreview(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	p := &doctorPage{}
	for _, v := range r.URL.Query()["id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		f, found, err := s.DB.Finding(r.Context(), id)
		if err != nil {
			log.Printf("web: finding: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found || !doctor.Fixable(f.Kind, f.Subject) {
			continue
		}
		pv, err := s.Previews.Preview(r.Context(), f)
		if err != nil {
			log.Printf("web: preview: %v", err)
			pv = doctor.Preview{Finding: f, Lines: []string{"Couldn't build a preview, so this can't be fixed right now: " + err.Error()}}
		}
		p.Previews = append(p.Previews, pv)
	}
	if len(p.Previews) == 0 {
		http.Redirect(w, r, "/doctor", http.StatusSeeOther)
		return
	}
	render(w, http.StatusOK, "doctor-preview", page{User: &u, Doctor: p})
}

// doctorApply queues what the preview page approved.
func (s *Server) doctorApply(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var fixes []storage.QueuedFix
	for _, v := range r.PostForm["id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		f, found, err := s.DB.Finding(r.Context(), id)
		if err != nil {
			log.Printf("web: finding: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found || !doctor.Fixable(f.Kind, f.Subject) {
			continue
		}
		choice := r.PostFormValue("choice-" + v)
		if choice == "" && doctor.NeedsChoice(f.Kind) {
			continue // left as it is
		}
		fixes = append(fixes, storage.QueuedFix{FindingID: id, Title: f.Title, Choice: choice})
	}
	// The whole batch or none of it, even if the browser gives up waiting.
	if err := s.DB.QueueFixes(context.WithoutCancel(r.Context()), u.Name, fixes); err != nil {
		log.Printf("web: queue fixes: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/doctor", http.StatusSeeOther)
}

func (s *Server) doctorCheck(w http.ResponseWriter, r *http.Request) {
	if s.DoctorNow != nil {
		s.DoctorNow()
	}
	http.Redirect(w, r, "/doctor", http.StatusSeeOther)
}

func (s *Server) setFinding(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if _, err := s.DB.SetFindingState(r.Context(), id, state); err != nil {
			log.Printf("web: set finding: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "doctor finding "+state, chi.URLParam(r, "id"), "")
		http.Redirect(w, r, "/doctor", http.StatusSeeOther)
	}
}

const logPageSize = 100

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	entries, err := s.DB.AuditLog(r.Context(), q, logPageSize+1, offset)
	if err != nil {
		log.Printf("web: audit log: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	p := &logPage{Entries: entries, Search: q}
	if len(entries) > logPageSize {
		p.Entries = entries[:logPageSize]
		p.More = "/log?" + url.Values{"q": {q}, "offset": {strconv.Itoa(offset + logPageSize)}}.Encode()
	}
	render(w, http.StatusOK, "log", page{User: &u, Log: p})
}

// recentDeletions is how many finished deletions the status screen shows.
const recentDeletions = 20

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	active, err := s.DB.ActiveDeletions(r.Context(), u)
	if err != nil {
		log.Printf("web: active deletions: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	recent, err := s.DB.RecentDeletions(r.Context(), u, recentDeletions)
	if err != nil {
		log.Printf("web: recent deletions: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	waiting, err := s.DB.EpisodeWaits(r.Context())
	if err != nil {
		log.Printf("web: episode waits: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "home", page{User: &u, Status: &statusPage{DryRun: s.DryRun, Active: storage.ByShow(active), Recent: storage.ByShow(recent), Waiting: waiting}})
}

func (s *Server) scheduleDeletion(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "ratingKey")
	section, _, found, err := s.DB.MediaInfo(r.Context(), key)
	if err != nil {
		log.Printf("web: media info: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	ok, err := s.DB.ScheduleDeletion(r.Context(), key, userFrom(r.Context()).SeerrID, s.DryRun, storage.KindManual)
	if err != nil {
		log.Printf("web: schedule deletion: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "set to keep, or already scheduled", http.StatusConflict)
		return
	}
	s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "schedule deletion", key, "")
	backToCard(w, r, section, key)
}

// changeDeletion runs a cancel or postpone. If the deletion moved on first
// there's nothing to change, and the page shows where it is now.
func (s *Server) changeDeletion(action string, change func(context.Context, string) (bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := chi.URLParam(r, "ratingKey")
		did, err := change(r.Context(), key)
		if err != nil {
			log.Printf("web: change deletion: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if did {
			s.DB.Audit(r.Context(), userFrom(r.Context()).Name, action, key, "")
		}
		section, _, found, err := s.DB.MediaInfo(r.Context(), key)
		if err != nil || !found { // a deletion can outlive its library item
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		backToCard(w, r, section, key)
	}
}

// Poster size: about the details card's width at 1.5x, for high-DPI screens.
const posterW, posterH = 240, 360

// poster serves a library item's poster from the disk cache, fetching it
// from Plex the first time. Any signed-in user may see any poster: artwork
// is not private, and the ownership rule is about actions.
func (s *Server) poster(w http.ResponseWriter, r *http.Request) {
	thumb, err := s.DB.Thumb(r.Context(), chi.URLParam(r, "ratingKey"))
	if err != nil {
		log.Printf("web: poster thumb: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if thumb == "" {
		http.NotFound(w, r)
		return
	}
	b, err := s.cachedPoster(r.Context(), thumb)
	if err != nil {
		log.Printf("web: poster: %v", err)
		http.Error(w, "poster unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(b) //nolint:gosec // G705: Plex image bytes served as image/jpeg with nosniff, never as HTML
}

// posterName is the thumb's cache file. The thumb path changes when the
// artwork does, so it names the file.
func posterName(thumb string) string {
	sum := sha256.Sum256([]byte(thumb))
	return hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(posterW) + ".jpg"
}

// cachedPoster returns a poster from disk, fetching it from Plex the first time.
func (s *Server) cachedPoster(ctx context.Context, thumb string) ([]byte, error) {
	name := posterName(thumb)
	if b, err := s.PosterCache.ReadFile(name); err == nil {
		return b, nil
	}
	b, err := s.Posters.Poster(ctx, thumb, posterW, posterH)
	if err != nil {
		return nil, err
	}
	// shortcut: the cache never shrinks; ~20 KB a poster, prune by hand if the library churns a lot.
	if err := writeAtomic(s.PosterCache, name, b); err != nil {
		log.Printf("web: cache poster: %v", err)
	}
	return b, nil
}

// warmFailures is how many posters in a row may fail before a run gives up:
// one broken poster shouldn't stop the rest, but Plex being down should.
const warmFailures = 5

// WarmPosters fetches every poster not cached yet, one at a time so Plex
// isn't flooded, and returns how many it fetched.
func (s *Server) WarmPosters(ctx context.Context) int {
	thumbs, err := s.DB.Thumbs(ctx)
	if err != nil {
		log.Printf("web: warm posters: %v", err)
		return 0
	}
	fetched, failed := 0, 0
	for _, thumb := range thumbs {
		if ctx.Err() != nil {
			return fetched
		}
		if _, err := s.PosterCache.Stat(posterName(thumb)); err == nil {
			continue
		}
		if _, err := s.cachedPoster(ctx, thumb); err != nil {
			if failed++; failed >= warmFailures {
				log.Printf("web: warm posters: stopped after %d failures: %v", failed, err)
				return fetched
			}
			continue
		}
		failed = 0
		fetched++
	}
	return fetched
}

// WarmLoop keeps the poster cache full until ctx ends.
func (s *Server) WarmLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n := s.WarmPosters(ctx); n > 0 {
			log.Printf("web: fetched %d posters", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	win, err := s.DB.DeleteWindow(r.Context())
	if err != nil {
		log.Printf("web: delete window: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	limit, err := s.DB.SpaceLimit(r.Context())
	if err != nil {
		log.Printf("web: space limit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	admin, err := s.DB.AdminDiscord(r.Context())
	if err != nil {
		log.Printf("web: admin discord: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expiry, err := s.DB.LogExpiry(r.Context())
	if err != nil {
		log.Printf("web: log expiry: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	level, err := s.DB.LogLevel(r.Context())
	if err != nil {
		log.Printf("web: log level: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	digestAt, err := s.DB.DigestTime(r.Context())
	if err != nil {
		log.Printf("web: digest time: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	paths, err := s.DB.LibraryPaths(r.Context())
	if err != nil {
		log.Printf("web: library paths: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	render(w, http.StatusOK, "settings", page{User: &u, Settings: &settingsPage{
		LogLevel: level, DefaultLevel: storage.DefaultLogLevel, DigestTime: digestAt, DefaultDigest: storage.DefaultDigestTime,
		Window: win, Zone: time.Now().Format("MST"), SpaceLimit: strconv.Itoa(limit), DefaultSpace: storage.DefaultSpaceLimit,
		DefaultWin: storage.DefaultWindow, AdminDiscord: admin, LogExpiry: strconv.Itoa(expiry), DefaultExpiry: storage.DefaultLogExpiry, Paths: paths, Saved: r.URL.Query().Get("saved") == "1",
	}})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	win := storage.DeleteWindow{Start: r.PostFormValue("window_start"), End: r.PostFormValue("window_end")}
	spaceForm := r.PostFormValue("space_limit")
	limit, err := storage.ParseSpaceLimit(spaceForm)
	if err == nil {
		err = win.Validate()
	}
	admin := strings.TrimSpace(r.PostFormValue("admin_discord"))
	if err == nil {
		err = storage.ValidAdminDiscord(admin)
	}
	expiryForm := r.PostFormValue("log_expiry")
	var expiry int
	if err == nil {
		expiry, err = storage.ParseLogExpiry(expiryForm)
	}
	digestForm := r.PostFormValue("digest_time")
	var digestAt string
	if err == nil {
		digestAt, err = storage.ParseDigestTime(digestForm)
	}
	levelForm := r.PostFormValue("log_level")
	var level string
	if err == nil {
		level, err = storage.ParseLogLevel(levelForm)
	}
	if err != nil {
		u := userFrom(r.Context())
		// The form must keep listing the paths, or the next save would unmanage them all.
		paths, perr := s.DB.LibraryPaths(r.Context())
		if perr != nil {
			log.Printf("web: library paths: %v", perr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// A 200: a typo here shouldn't count towards a proxy ban.
		render(w, http.StatusOK, "settings", page{User: &u, Error: err.Error(), Settings: &settingsPage{
			LogLevel: levelForm, DefaultLevel: storage.DefaultLogLevel, DigestTime: digestForm, DefaultDigest: storage.DefaultDigestTime,
			Window: win, Zone: time.Now().Format("MST"), SpaceLimit: spaceForm, DefaultSpace: storage.DefaultSpaceLimit,
			DefaultWin: storage.DefaultWindow, AdminDiscord: admin, LogExpiry: expiryForm, DefaultExpiry: storage.DefaultLogExpiry, Paths: paths,
		}})
		return
	}
	err = s.DB.SetDeleteWindow(r.Context(), win)
	if err == nil {
		err = s.DB.SetSpaceLimit(r.Context(), limit)
	}
	if err == nil {
		err = s.DB.SetAdminDiscord(r.Context(), admin)
	}
	if err == nil {
		err = s.DB.SetLogExpiry(r.Context(), expiry)
	}
	if err == nil {
		err = s.DB.SetLogLevel(r.Context(), level)
	}
	if err == nil {
		err = s.DB.SetDigestTime(r.Context(), digestAt)
	}
	if err == nil {
		err = s.DB.SetManagedPaths(r.Context(), r.PostForm["managed"])
	}
	if err != nil {
		log.Printf("web: save settings: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.DB.Audit(r.Context(), userFrom(r.Context()).Name, "save settings", "", "")
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// writeAtomic writes name in root via a temp file and rename, so a reader
// never sees a half-written file.
func writeAtomic(root *os.Root, name string, b []byte) error {
	rnd := make([]byte, 8)
	_, _ = rand.Read(rnd)
	tmp := "." + name + "." + hex.EncodeToString(rnd)
	if err := root.WriteFile(tmp, b, 0o600); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, name)
}

// requireAdmin refuses non-admins; it runs after requireSession.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !userFrom(r.Context()).IsAdmin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginLocal(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	email := strings.TrimSpace(r.PostFormValue("email"))
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		render(w, http.StatusTooManyRequests, "login", page{Error: "too many failed sign-ins; wait 15 minutes", Email: email})
		return
	}
	id, err := s.Seerr.LocalLogin(r.Context(), email, r.PostFormValue("password"))
	if errors.Is(err, seerr.ErrBadCredentials) {
		s.limiter.fail(ip)
		render(w, http.StatusUnauthorized, "login", page{Error: "wrong email or password", Email: email})
		return
	}
	if err != nil {
		log.Printf("web: seerr local login: %v", err)
		render(w, http.StatusBadGateway, "login", page{Error: "could not reach Seerr", Email: email})
		return
	}
	// Permissions come from the API-key view of the user, not the login reply.
	u, err := s.Seerr.User(r.Context(), id)
	if err != nil {
		log.Printf("web: seerr user %d: %v", id, err) //nolint:gosec // G706: id is an int64 and err is this app's own wrapping, no user text
		render(w, http.StatusBadGateway, "login", page{Error: "could not reach Seerr", Email: email})
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) loginPlex(w http.ResponseWriter, r *http.Request) {
	pin, err := s.PlexTV.CreatePin(r.Context())
	if err != nil {
		log.Printf("web: plex pin: %v", err)
		render(w, http.StatusBadGateway, "login", page{Error: "could not reach plex.tv"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: pinCookie, Value: strconv.FormatInt(pin.ID, 10) + "." + pin.Code, Path: "/login/plex",
		MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	cb := (&url.URL{Scheme: "https", Host: r.Host, Path: "/login/plex/callback"}).String()
	http.Redirect(w, r, s.PlexTV.AuthURL(pin, cb), http.StatusSeeOther)
}

func (s *Server) loginPlexCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(pinCookie)
	idStr, code, ok := strings.Cut(valueOf(c, err), ".")
	id, perr := strconv.ParseInt(idStr, 10, 64)
	if !ok || perr != nil || code == "" {
		render(w, http.StatusBadRequest, "login", page{Error: "Plex sign-in expired; try again"})
		return
	}
	clearCookie(w, pinCookie, "/login/plex")
	pin := plex.Pin{ID: id, Code: code}

	var token string
	for deadline := time.Now().Add(s.PinWait); ; {
		if token, err = s.PlexTV.PinToken(r.Context(), pin); err != nil || token != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil || token == "" {
		log.Printf("web: plex pin %d: token=%t err=%v", id, token != "", err)
		render(w, http.StatusUnauthorized, "login", page{Error: "Plex sign-in was not completed"})
		return
	}
	acct, err := s.PlexTV.Account(r.Context(), token)
	if err != nil {
		log.Printf("web: plex account: %v", err)
		render(w, http.StatusBadGateway, "login", page{Error: "could not reach plex.tv"})
		return
	}
	u, found, err := s.Seerr.UserByPlexID(r.Context(), acct.ID)
	if err != nil {
		log.Printf("web: seerr users: %v", err)
		render(w, http.StatusBadGateway, "login", page{Error: "could not reach Seerr"})
		return
	}
	if !found {
		render(w, http.StatusForbidden, "login", page{Error: "your Plex account (" + acct.Username + ") is not a Seerr user; ask the admin to import it"})
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, su seerr.User) {
	u := storage.User{SeerrID: su.ID, Name: su.Name(), IsAdmin: su.IsAdmin()}
	if su.PlexID != nil {
		u.PlexID.Int64, u.PlexID.Valid = *su.PlexID, true
	}
	if err := s.DB.UpsertUser(r.Context(), u); err != nil {
		log.Printf("web: save user: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, err := s.DB.CreateSession(r.Context(), u.SeerrID, sessionTTL)
	if err != nil {
		log.Printf("web: create session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.DB.DeleteSession(r.Context(), c.Value); err != nil {
			log.Printf("web: delete session: %v", err)
		}
	}
	clearCookie(w, sessionCookie, "/")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type ctxKey struct{}

func userFrom(ctx context.Context) storage.User { return ctx.Value(ctxKey{}).(storage.User) }

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		u, err := s.DB.SessionUser(r.Context(), c.Value)
		if errors.Is(err, storage.ErrNoSession) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			log.Printf("web: session lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// sameOrigin rejects cross-site form posts (CSRF). Browsers send Origin on
// every POST; a request without one isn't from a browser form.
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Path: path, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func valueOf(c *http.Cookie, err error) string {
	if err != nil {
		return ""
	}
	return c.Value
}

// limiter counts failed sign-ins per client address: 10 in 15 minutes
// blocks that address until the oldest failure ages out.
// shortcut: behind a reverse proxy every client shares the proxy's address, so the
// limit is effectively global; read X-Forwarded-For from a trusted proxy if that bites.
type limiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

const (
	maxFails   = 10
	failWindow = 15 * time.Minute
)

func (l *limiter) recent(key string) []time.Time {
	cut := time.Now().Add(-failWindow)
	var keep []time.Time
	for _, t := range l.fails[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.fails, key)
	} else {
		l.fails[key] = keep
	}
	return keep
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(key)) >= maxFails
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string][]time.Time{}
	}
	l.fails[key] = append(l.recent(key), time.Now())
}
