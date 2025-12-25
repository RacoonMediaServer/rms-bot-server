package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	rms_users "github.com/RacoonMediaServer/rms-packages/pkg/service/rms-users"
	"github.com/gorilla/websocket"
	"go-micro.dev/v4/logger"
)

var upgrader = websocket.Upgrader{
	HandshakeTimeout: 10 * time.Second,
}

type authResult struct {
	userId  string
	token   string
	selfReg bool
}

func (e *endpoint) authorize(ctx context.Context, token string) (authResult, error) {
	if token == "" {
		return authResult{}, errors.New("invalid empty token")
	}

	req := rms_users.CheckPermissionsRequest{
		Token:  token,
		Perms:  []rms_users.Permissions{rms_users.Permissions_ConnectingToTheBot},
		Domain: &e.domain,
	}

	resp, err := e.f.NewUsers().CheckPermissions(ctx, &req)
	if err != nil {
		return authResult{}, err
	}

	if !resp.Allowed {
		return authResult{}, errors.New("access denied")
	}

	result := authResult{
		userId: resp.UserId,
		token:  token,
	}

	return result, nil
}

func (e *endpoint) handler(w http.ResponseWriter, r *http.Request) {
	l := e.l.Fields(map[string]interface{}{"addr": r.RemoteAddr})
	for key, val := range r.Header {
		l.Logf(logger.InfoLevel, "Got header %s = %+v", key, val)
	}
	token := r.Header.Get("X-Token")
	result, err := e.authorize(r.Context(), token)
	if err != nil {
		l.Logf(logger.ErrorLevel, "Forbidden: %s", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		l.Logf(logger.ErrorLevel, "Upgrade connection failed: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	sess := newSession(e.l, conn, result.userId, e.ch)
	defer sess.close()

	notifyAboutConnect := false
	e.mu.Lock()
	if existing, ok := e.sessions[result.userId]; ok {
		existing.drop()
	}
	e.sessions[result.userId] = sess
	_, pending := e.disconnectedAt[result.userId]
	if pending {
		delete(e.disconnectedAt, result.userId)
	}
	notifyAboutConnect = !pending
	e.mu.Unlock()

	if notifyAboutConnect {
		e.ch <- getDeviceConnectedMessage(result.userId)
	}

	sess.run(r.Context())

	e.mu.Lock()
	if existing, ok := e.sessions[result.userId]; ok {
		if existing == sess {
			delete(e.sessions, result.userId)
			e.disconnectedAt[result.userId] = time.Now()
		}
	}
	e.mu.Unlock()
}
