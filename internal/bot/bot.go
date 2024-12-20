package bot

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/RacoonMediaServer/rms-bot-server/internal/comm"
	"github.com/RacoonMediaServer/rms-bot-server/internal/model"
	"github.com/RacoonMediaServer/rms-packages/pkg/communication"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"go-micro.dev/v4/logger"
)

// Database is a set of methods for accessing table, which map devices to Telegram users
type Database interface {
	LoadLinks() (result map[string][]*model.Link, err error)
	AddLink(link *model.Link) error
	DelLink(link *model.Link) error
}

// Bot implements a Telegram bot
type Bot struct {
	l   logger.Logger
	wg  sync.WaitGroup
	api *tgbotapi.BotAPI
	cmd chan interface{}
	c   comm.DeviceCommunicator
	db  Database

	deviceToUsers map[string][]*model.Link
	chatToDevice  map[int64]string

	codeToDevice map[string]linkageCode
	deviceToCode map[string]string
}

type stopCommand struct{}

func NewBot(token string, database Database, c comm.DeviceCommunicator) (*Bot, error) {
	var err error
	bot := &Bot{
		l:  logger.DefaultLogger.Fields(map[string]interface{}{"from": "bot"}),
		db: database,
		c:  c,

		deviceToUsers: map[string][]*model.Link{},
		chatToDevice:  map[int64]string{},
		codeToDevice:  map[string]linkageCode{},
		deviceToCode:  map[string]string{},
	}

	bot.deviceToUsers, err = database.LoadLinks()
	if err != nil {
		return nil, fmt.Errorf("load linkages failed: %w", err)
	}
	for id, links := range bot.deviceToUsers {
		for _, link := range links {
			bot.chatToDevice[link.TgChatID] = id
		}
	}

	bot.api, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}

	bot.cmd = make(chan interface{})
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
				bot.generateLinkageCode(msgToUser.DeviceID)
			} else if msgToUser.Message.Type == communication.MessageType_UnlinkUser {
				bot.unlinkUserFromDevice(int(msgToUser.Message.User), msgToUser.DeviceID)
			}

		case <-ticker.C:
			// чистим просроченные коды
			bot.clearExpiredLinkageCodes()

		case update := <-updates:
			var message *tgbotapi.Message
			if update.Message == nil {
				if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
					message = update.CallbackQuery.Message
					message.Text = update.CallbackQuery.Data
					message.From = update.CallbackQuery.From
				}
			} else {
				message = update.Message
				if update.Message.From != nil {
					incomingMessagesCounter.WithLabelValues(update.Message.From.UserName).Inc()
				}
			}
			if message == nil || message.From.ID == bot.api.Self.ID {
				continue
			}
			bot.sendMessageToDevice(message)

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

func (bot *Bot) send(msg tgbotapi.Chattable) int {
	tgMsg, err := bot.api.Send(msg)
	if err != nil {
		bot.l.Logf(logger.ErrorLevel, "Send message failed: %s", err)
		return 0
	}
	return tgMsg.MessageID
}

func (bot *Bot) sendMessageToUser(message comm.OutgoingMessage) {
	links, ok := bot.deviceToUsers[message.DeviceID]
	if !ok {
		return
	}
	for _, u := range links {
		if message.Message.User == 0 || message.Message.User == int32(u.TgUserID) {
			msg := deserializeMessage(u.TgChatID, message.Message)
			msgId := bot.send(msg)
			if message.Message.Pin == communication.BotMessage_ThisMessage {
				cfg := tgbotapi.PinChatMessageConfig{
					ChatID:    u.TgChatID,
					MessageID: msgId,
				}
				_, err := bot.api.PinChatMessage(cfg)
				if err != nil {
					bot.l.Logf(logger.WarnLevel, "Pin message %d failed: %s", msgId, err)
				}
			} else if message.Message.Pin == communication.BotMessage_Drop {
				params := url.Values{}
				params.Add("chat_id", strconv.FormatInt(u.TgChatID, 10))
				_, err := bot.api.MakeRequest("unpinAllChatMessages", params)
				if err != nil {
					bot.l.Logf(logger.WarnLevel, "Unpin message failed: %s", err)
				}
			}
		}
	}
}

func (bot *Bot) sendMessageToDevice(message *tgbotapi.Message) {
	bot.l.Logf(logger.DebugLevel, "Got message from Telegram: '%s' [@%s, %d]", message.Text, message.From.UserName, message.From.ID)
	token, ok := bot.chatToDevice[message.Chat.ID]
	if !ok {
		if code, ok := bot.codeToDevice[message.Text]; ok {
			if err := bot.linkUserToDevice(message.From, message.Chat.ID, code); err != nil {
				bot.l.Logf(logger.ErrorLevel, "Link %d to device %s failed: %s", message.Chat.ID, code.device, err)
				bot.sendTextMessage(message.Chat.ID, "Не удалось связать чат с устройством")
				return
			}

			bot.l.Logf(logger.InfoLevel, "Device '%s' linked to chat %d", code.device, message.Chat.ID)
			bot.sendTextMessage(message.Chat.ID, "Текущий чат связан с устройством. Ура, ничего не сломалось...")
			return
		}
		bot.l.Logf(logger.WarnLevel, "Unlinked message, user id = %s [%d], text = '%s'", message.From.UserName, message.From.ID, message.Text)
		bot.sendTextMessage(message.Chat.ID, "Необходимо привязать устройство к текущему чату. Для этого необходимо ввести здесь код из веб-интерфейса")
		return
	}

	msg := bot.serializeMessage(message)
	if err := bot.c.Send(comm.IncomingMessage{DeviceID: token, Message: msg}); err != nil {
		bot.l.Logf(logger.ErrorLevel, "Cannot send message to the device: %s", err)
		text := ""
		if errors.Is(err, comm.ErrDeviceIsNotConnected) {
			text = "Устройство не в сети, команда не доставлена"
		} else {
			text = "Что-то пошло не так..."
		}
		bot.sendTextMessage(message.Chat.ID, text)
	}
}

func (bot *Bot) sendTextMessage(chat int64, text string) {
	msg := newMessage(text, 0)
	bot.send(msg.compose(chat))
}
