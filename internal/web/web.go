// Package web serves the server-rendered, terminal-themed UI.
package web

import (
	"context"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/voidgrid/voidgrid-cleaner/internal/clients/plex"
	"github.com/voidgrid/voidgrid-cleaner/internal/clients/seerr"
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
	DB     *storage.DB
	Seerr  *seerr.Client
	PlexTV *plex.TV
	// PinWait is how long the Plex callback waits for plex.tv to report
	// the pin approved (it can lag the redirect slightly).
	PinWait time.Duration

	limiter limiter
}

type page struct {
	User  *storage.User
	Error string
	Email string
}

var pages = map[string]*template.Template{}

func init() {
	for _, name := range []string{"login", "home"} {
		pages[name] = template.Must(template.New("layout").ParseFS(webassets.FS, "templates/layout.html", "templates/"+name+".html"))
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
	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			u := userFrom(r.Context())
			render(w, http.StatusOK, "home", page{User: &u})
		})
	})
	return r
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
