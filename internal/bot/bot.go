package bot

import (
	"errors"
	"github.com/RacoonMediaServer/rms-bot-server/internal/comm"
	"github.com/RacoonMediaServer/rms-packages/pkg/communication"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"go-micro.dev/v4/logger"
)

// Database is a set of methods for accessing table, which map devices to Telegram users
type Database interface {
}

// Bot implements a Telegram bot
type Bot struct {
	l            logger.Logger
	wg           sync.WaitGroup
	api          *tgbotapi.BotAPI
	cmd          chan interface{}
	c            comm.DeviceCommunicator
	db           Database
	tokenToChat  map[string]int64
	chatToToken  map[int64]string
	linkageCodes map[string]linkageCode
}

type stopCommand struct{}

func NewBot(token string, database Database, c comm.DeviceCommunicator) (*Bot, error) {
	bot := &Bot{
		l:            logger.DefaultLogger.Fields(map[string]interface{}{"from": "bot"}),
		db:           database,
		cmd:          make(chan interface{}),
		c:            c,
		tokenToChat:  map[string]int64{},
		chatToToken:  map[int64]string{},
		linkageCodes: map[string]linkageCode{},
	}

	// TODO: загрузка чатов из БД

	var err error
	bot.api, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	bot.wg.Add(1)
	go func() {
		defer bot.wg.Done()
		bot.loop()
	}()

	return bot, nil
}

func (bot *Bot) loop() {
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60

	updates, err := bot.api.GetUpdatesChan(updateConfig)

	if err != nil {
		logger.Errorf("Get bot updates failed: %+v", err)
		return
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msgToUser := <-bot.c.OutgoingChannel():
			if msgToUser.Message.Type == communication.MessageType_Interaction {
				bot.sendMessageToUser(msgToUser)
			} else if msgToUser.Message.Type == communication.MessageType_AcquiringCode {
				bot.generateLinkageCode(msgToUser.Token)
			}

		case <-ticker.C:
			// чистим просроченные коды
			bot.clearExpiredLinkageCodes()

		case update := <-updates:
			if update.Message == nil {
				continue
			}
			bot.sendMessageToDevice(update.Message)

		case command := <-bot.cmd:
			switch command.(type) {
			case *stopCommand:
				return
			default:
			}
		}
	}
}

func (bot *Bot) Stop() {
	bot.cmd <- &stopCommand{}
	bot.wg.Wait()
}

func (bot *Bot) send(msg tgbotapi.Chattable) {
	_, err := bot.api.Send(msg)
	if err != nil {
		bot.l.Logf(logger.ErrorLevel, "Send message failed: %s", err)
	}
}
func (bot *Bot) sendMessageToUser(message comm.OutgoingMessage) {
	chat, ok := bot.tokenToChat[message.Token]
	if !ok {
		return
	}
	msg := deserializeMessage(chat, message.Message)
	bot.send(msg)
}

func (bot *Bot) sendMessageToDevice(message *tgbotapi.Message) {
	token, ok := bot.chatToToken[message.Chat.ID]
	if !ok {
		if code, ok := bot.linkageCodes[message.Text]; ok {
			bot.linkUserToDevice(message.Chat.ID, code)

			msg := newMessage("Текущий чат связан с устройством. Ура, ничего не сломалось...", 0)
			bot.send(msg.compose(message.Chat.ID))
			return
		}
		msg := newMessage("Необходимо привязать устройство к текущему чату. Для этого необходимо ввести здесь код из веб-интерфейса", 0)
		bot.send(msg.compose(message.Chat.ID))
		return
	}

	msg := serializeMessage(message)
	if err := bot.c.Send(comm.IncomingMessage{Token: token, Message: msg}); err != nil {
		bot.l.Logf(logger.ErrorLevel, "Cannot send message to the device: %s", err)
		text := ""
		if errors.Is(err, comm.ErrDeviceIsNotConnected) {
			text = "Устройство не в сети, команда не доставлена"
		} else {
			text = "Что-то пошло не так..."
		}

		msg := newMessage(text, 0)
		bot.send(msg.compose(message.Chat.ID))
	}
}