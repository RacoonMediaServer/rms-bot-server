package server

import (
	"sync"
	"time"

	"github.com/RacoonMediaServer/rms-bot-server/internal/comm"
	"github.com/RacoonMediaServer/rms-packages/pkg/service/servicemgr"
	"go-micro.dev/v4/logger"
)

const (
	checkDisconnectedSessionInterval  = 5 * time.Second
	disconnectedSessionNotifyInterval = 30 * time.Second
)

type endpoint struct {
	l              logger.Logger
	f              servicemgr.ServiceFactory
	domain         string
	ch             chan comm.OutgoingMessage
	t              *time.Ticker
	mu             sync.RWMutex
	sessions       map[string]*session
	disconnectedAt map[string]time.Time
}

func newEndpoint(l logger.Logger, f servicemgr.ServiceFactory, domain string) *endpoint {
	e := &endpoint{
		l:              l,
		f:              f,
		t:              time.NewTicker(checkDisconnectedSessionInterval),
		domain:         domain,
		sessions:       make(map[string]*session),
		disconnectedAt: make(map[string]time.Time),
		ch:             make(chan comm.OutgoingMessage, maxMessageQueueSize),
	}

	go e.statusNotifier()

	return e
}

func (e *endpoint) statusNotifier() {
	for range e.t.C {
		now := time.Now()
		e.mu.Lock()
		toDelete := make([]string, 0, len(e.disconnectedAt))
		for user, disconnectedAt := range e.disconnectedAt {
			if now.Sub(disconnectedAt) >= disconnectedSessionNotifyInterval {
				e.ch <- getDeviceDisconnectedMessage(user)
				toDelete = append(toDelete, user)
			}
		}
		for _, user := range toDelete {
			delete(e.disconnectedAt, user)
		}
		e.mu.Unlock()
	}
}

func (e *endpoint) OutgoingChannel() <-chan comm.OutgoingMessage {
	return e.ch
}

func (e *endpoint) Send(message comm.IncomingMessage) error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	sess, ok := e.sessions[message.DeviceID]
	if !ok {
		return comm.ErrDeviceIsNotConnected
	}

	sess.send(message.Message)

	return nil
}

func (e *endpoint) dropSession(user string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	sess, ok := e.sessions[user]
	if !ok {
		return
	}

	sess.drop()
	delete(e.sessions, user)
}
