package server

import (
	"context"
	"sync"
	"time"

	"github.com/RacoonMediaServer/rms-bot-server/internal/comm"
	"github.com/RacoonMediaServer/rms-packages/pkg/communication"
	"github.com/gorilla/websocket"
	"go-micro.dev/v4/logger"
	"google.golang.org/protobuf/proto"
)

type session struct {
	l      logger.Logger
	conn   *websocket.Conn
	userId string
	user   chan *communication.UserMessage
	out    chan<- comm.OutgoingMessage
}

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second // < pongWait
)

func newSession(l logger.Logger, conn *websocket.Conn, userId string, out chan<- comm.OutgoingMessage) *session {
	return &session{
		l:      l.Fields(map[string]interface{}{"userId": userId}),
		conn:   conn,
		userId: userId,
		user:   make(chan *communication.UserMessage, maxMessageQueueSize),
		out:    out,
	}
}

func (s *session) run(ctx context.Context) {
	s.l.Log(logger.InfoLevel, "Established")
	sessionsGauge.Inc()
	defer sessionsGauge.Dec()

	wg := sync.WaitGroup{}
	wg.Add(1)
	defer wg.Wait()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.conn.SetReadDeadline(time.Now().Add(pongWait))
	s.conn.SetPongHandler(func(string) error {
		s.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	go func() {
		defer wg.Done()
		s.writeProcess(ctx)
	}()

	for {
		// читаем сообщения от клиентского устройства
		_, buf, err := s.conn.ReadMessage()
		if err != nil {
			s.l.Logf(logger.ErrorLevel, "pick message failed: %s", s.userId, err)
			return
		}
		msg := &communication.BotMessage{}
		if err = proto.Unmarshal(buf, msg); err != nil {
			s.l.Logf(logger.ErrorLevel, "deserialize incoming message failed: %s", err)
			return
		}
		s.l.Logf(logger.DebugLevel, "message from device received: %s", msg)
		s.out <- comm.OutgoingMessage{DeviceID: s.userId, Message: msg}
		outgoingMessagesCounter.WithLabelValues(s.userId).Inc()
	}
}

func (s *session) writeProcess(ctx context.Context) {
	pingTimer := time.NewTicker(pingPeriod)
	defer pingTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-s.user:
			buf, err := proto.Marshal(msg)
			if err != nil {
				s.l.Logf(logger.ErrorLevel, "serialize message failed: %s", err)
				continue
			}
			s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err = s.conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
				s.l.Logf(logger.ErrorLevel, "write message failed: %s", err)
				continue
			}
			s.l.Logf(logger.DebugLevel, "message sent to device: %s", msg)

		case <-pingTimer.C:
			s.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := s.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				s.l.Logf(logger.ErrorLevel, "send ping failed: %s", err)
				return
			}
		}
	}
}

func (s *session) send(msg *communication.UserMessage) {
	s.user <- msg
}

func (s *session) drop() {
	_ = s.conn.Close()
}

func (s *session) close() {
	s.drop()
	close(s.user)
}
