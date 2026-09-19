package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

type heldAdmission struct {
	user    int32
	kind    string
	entered bool
	release chan struct{}
	resumed bool
}
type controls struct {
	mu                  sync.Mutex
	store               *store.Store
	token               string
	holds               map[string]*heldAdmission
	faultID, faultPoint string
}

func newControls(s *store.Store, token string) *controls {
	return &controls{store: s, token: token, holds: map[string]*heldAdmission{}}
}

func (c *controls) beforePublication(ctx context.Context, user int32, kind string) error {
	c.mu.Lock()
	var hold *heldAdmission
	for _, candidate := range c.holds {
		if candidate.user == user && candidate.kind == kind && !candidate.entered {
			candidate.entered = true
			hold = candidate
			break
		}
	}
	c.mu.Unlock()
	if hold == nil {
		return nil
	}
	select {
	case <-hold.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *controls) fault(id, point string) {
	c.mu.Lock()
	crash := c.faultID == id && c.faultPoint == point
	c.mu.Unlock()
	if crash {
		os.Exit(86)
	} // Deliberately skip cleanup at the declared SQLite boundary.
}

func (c *controls) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(c.token)) != 1 {
		http.Error(w, "denied", 403)
		return
	}
	var request struct {
		Action    string `json:"action"`
		Subject   string `json:"subject"`
		Kind      string `json:"kind"`
		Handle    string `json:"handle"`
		Operation string `json:"operation"`
		Point     string `json:"point"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid control", 400)
		return
	}
	result, err := c.apply(r.Context(), request.Action, request.Subject, request.Kind, request.Handle, request.Operation, request.Point)
	if err != nil {
		http.Error(w, "control refused", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (c *controls) apply(ctx context.Context, action, subject, kind, handle, operation, point string) (any, error) {
	var userID int32
	if action == "arm" {
		s, _, err := c.store.GetDriver().(store.ShrimpDriver).ReadShrimp(ctx, subject, nil)
		if err != nil || s == nil || s.ID != subject {
			return nil, errors.New("unknown pilot subject")
		}
		userID = s.UserID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch action {
	case "arm":
		if kind != "session" && kind != "refresh" && kind != "pat" {
			return nil, errors.New("unsupported admission kind")
		}
		if len(c.holds) >= 32 {
			return nil, errors.New("hold limit")
		}
		for _, old := range c.holds {
			if old.user == userID && old.kind == kind && !old.resumed {
				return nil, errors.New("already held")
			}
		}
		handle = random.UUID()
		c.holds[handle] = &heldAdmission{user: userID, kind: kind, release: make(chan struct{})}
		return map[string]any{"handle": handle}, nil
	case "status", "resume":
		hold, ok := c.holds[handle]
		if !ok {
			return nil, errors.New("unknown hold")
		}
		if action == "resume" && !hold.resumed {
			close(hold.release)
			hold.resumed = true
		}
		return map[string]any{"held": hold.entered, "resumed": hold.resumed}, nil
	case "crash":
		if operation == "" || (point != "before_commit" && point != "after_commit") {
			return nil, errors.New("invalid crash boundary")
		}
		c.faultID = operation
		c.faultPoint = point
		return map[string]any{"armed": true}, nil
	default:
		return nil, errors.New("unknown control")
	}
}
