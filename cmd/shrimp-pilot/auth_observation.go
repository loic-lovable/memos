package main

import (
	"context"
	"net/http"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/server/auth"
	"github.com/usememos/memos/store"
)

const observationHeader = "X-Memos-Pilot-Observation"

type authObservation struct {
	user                     int32
	subject, path            string
	expires                  time.Time
	requests, callbacks      int
	matched, completed, read bool
	status                   int
}

// armAuth binds a one-use private handle to the actual pilot subject's native
// PAT creation route. Merely finding an archived user here proves nothing.
func (c *controls) armAuth(ctx context.Context, subject, kind string) (any, error) {
	if kind != "pat" {
		return nil, errors.New("unsupported authentication observation")
	}
	s, _, err := c.store.GetDriver().(store.ShrimpDriver).ReadShrimp(ctx, subject, nil)
	if err != nil || s == nil || s.ID != subject {
		return nil, errors.New("unknown pilot subject")
	}
	user, err := c.store.GetUser(ctx, &store.FindUser{ID: &s.UserID})
	if err != nil || user == nil {
		return nil, errors.New("unknown native user")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.observations) >= 32 {
		return nil, errors.New("observation limit")
	}
	handle := random.UUID()
	c.observations[handle] = &authObservation{user: user.ID, subject: subject,
		path: "/api/v1/users/" + user.Username + "/personalAccessTokens", expires: time.Now().Add(time.Minute)}
	return map[string]any{"handle": handle}, nil
}

// readAuth exposes only bounded evidence through the TLS control-token endpoint.
// Missing, repeated, expired, ambiguous or incomplete observations prove nothing.
func (c *controls) readAuth(handle string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.observations[handle]
	if o == nil {
		return nil, errors.New("unknown observation")
	}
	outcome := "unavailable"
	if !o.read && o.completed && o.requests == 1 && o.callbacks == 1 && o.matched &&
		o.status == http.StatusUnauthorized && time.Now().Before(o.expires) {
		outcome = "archived_access_token"
	}
	o.read = true
	return map[string]any{"handle": handle, "subject": o.subject, "kind": "pat", "outcome": outcome}, nil
}

// observeRequests never changes a public response. The opaque handle can only
// be minted and inspected with the separate control credential. The context hook
// records the original authentication decision, without replaying credentials or
// inferring why a request failed from a later database read.
func (c *controls) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handles := r.Header.Values(observationHeader)
		r.Header.Del(observationHeader)
		if len(handles) != 1 {
			// Multiple correlation values cannot leave a previously claimed handle
			// usable: count any referenced known handle as ambiguous.
			c.mu.Lock()
			for _, handle := range handles {
				if o := c.observations[handle]; o != nil {
					o.requests += 2
					o.completed = true
				}
			}
			c.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		c.mu.Lock()
		o := c.observations[handles[0]]
		if o == nil {
			c.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		o.requests++
		eligible := o.requests == 1 && !o.read && time.Now().Before(o.expires) && r.TLS != nil &&
			r.Method == http.MethodPost && r.URL.Path == o.path && r.URL.RawQuery == "" &&
			len(r.Header.Values("Authorization")) == 1 && len(r.Header.Values("Cookie")) == 0
		if !eligible {
			o.completed = true
			c.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}
		c.mu.Unlock()
		ctx := auth.WithArchivedAccessTokenObserver(r.Context(), func(userID int32) {
			c.mu.Lock()
			defer c.mu.Unlock()
			o.callbacks++
			o.matched = userID == o.user
		})
		response := &observedResponse{ResponseWriter: w}
		next.ServeHTTP(response, r.WithContext(ctx))
		c.mu.Lock()
		o.status, o.completed = response.status, true
		c.mu.Unlock()
	})
}

type observedResponse struct {
	http.ResponseWriter
	status int
}

func (w *observedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedResponse) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}
