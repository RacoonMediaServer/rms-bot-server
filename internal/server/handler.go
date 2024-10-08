package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	rms_users "github.com/RacoonMediaServer/rms-packages/pkg/service/rms-users"
	"github.com/gorilla/websocket"
	"go-micro.dev/v4/logger"
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
}

func (s *Server) authorize(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}

	resp, err := s.f.NewUsers().GetPermissions(ctx, &rms_users.GetPermissionsRequest{Token: token})
	if err != nil {
		return false, fmt.Errorf("token validation failed: %w", err)
	}
	for _, perm := range resp.Perms {
		if perm == rms_users.Permissions_ConnectingToTheBot {
			return true, nil
		}
	}

	return false, nil
}

func (s *Server) handler(w http.ResponseWriter, r *http.Request) {
	l := s.l.Fields(map[string]interface{}{"addr": r.RemoteAddr})
	for key, val := range r.Header {
		l.Logf(logger.InfoLevel, "Got header %s = %+v", key, val)
	}
	token := r.Header.Get("X-Token")
	if ok, err := s.authorize(r.Context(), token); !ok {
		if err != nil {
			l.Logf(logger.ErrorLevel, "Authorization failed: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		l.Log(logger.ErrorLevel, "Forbidden")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		l.Logf(logger.ErrorLevel, "Upgrade connection failed: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	sess := newSession(s.l, conn, token, s.ch)
	defer sess.close()

	s.mu.Lock()
	if existing, ok := s.sessions[token]; ok {
		existing.drop()
	}
	s.sessions[token] = sess
	s.mu.Unlock()

	sess.run(r.Context())

	s.mu.Lock()
	if existing, ok := s.sessions[token]; ok {
		if existing == sess {
			delete(s.sessions, token)
		}
	}
	s.mu.Unlock()
}
