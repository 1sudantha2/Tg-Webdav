// Package telegram wires the gotd/td MTProto client to the virtual file
// system: it authenticates the bot, resolves the storage channel, listens to
// the bot inbox for file ingestion and provides the streaming upload /
// download primitives used by the WebDAV and Web UI layers.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"

	"github.com/1sudantha2/Tg-Webdav/internal/config"
	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/stream"
)

// Service is the Telegram backend of the virtual file system.
type Service struct {
	cfg   *config.Config
	store *db.DB
	cache *stream.ChunkCache

	dispatcher tg.UpdateDispatcher
	client     *telegram.Client

	mu      sync.RWMutex
	api     *tg.Client
	sender  *message.Sender
	channel tg.InputPeerClass // resolved storage channel
}

// New creates the Telegram service. Call Run to connect and authenticate.
func New(cfg *config.Config, store *db.DB, cache *stream.ChunkCache) *Service {
	return &Service{
		cfg:        cfg,
		store:      store,
		cache:      cache,
		dispatcher: tg.NewUpdateDispatcher(),
	}
}

// Run connects to Telegram, authenticates the bot and blocks until ctx is
// canceled or a fatal error occurs.
func (s *Service) Run(ctx context.Context) error {
	s.client = telegram.NewClient(s.cfg.TelegramAPIID, s.cfg.TelegramAPIHash, telegram.Options{
		UpdateHandler:  s.dispatcher,
		SessionStorage: &session.FileStorage{Path: s.cfg.SessionPath},
	})

	return s.client.Run(ctx, func(ctx context.Context) error {
		api := s.client.API()

		if err := s.authorize(ctx); err != nil {
			return err
		}

		s.mu.Lock()
		s.api = api
		s.sender = message.NewSender(api)
		s.mu.Unlock()

		s.registerUpdateHandlers()
		go s.resolveChannelLoop(ctx)

		slog.Info("telegram connected", "session", s.cfg.SessionPath)
		<-ctx.Done()
		return ctx.Err()
	})
}

// authorize performs bot authentication, reusing the persisted session when
// possible.
func (s *Service) authorize(ctx context.Context) error {
	status, err := s.client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("auth status: %w", err)
	}
	if status.Authorized {
		return nil
	}
	if _, err := auth.NewClient(s.client.API(), nil, s.cfg.TelegramAPIID, s.cfg.TelegramAPIHash).
		Bot(ctx, s.cfg.TelegramBotToken); err != nil {
		return fmt.Errorf("bot login failed (check TELEGRAM_API_ID / TELEGRAM_API_HASH / TELEGRAM_BOT_TOKEN): %w", err)
	}
	slog.Info("telegram bot authorized")
	return nil
}

// apiNow returns the raw MTProto client or an error when not connected yet.
func (s *Service) apiNow() (*tg.Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.api == nil {
		return nil, fmt.Errorf("telegram not connected yet")
	}
	return s.api, nil
}

// ---------------------------------------------------------------------------
// Storage channel resolution
// ---------------------------------------------------------------------------

// channelPeer returns the resolved storage channel input peer.
func (s *Service) channelPeer() (tg.InputPeerClass, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.channel == nil {
		return nil, fmt.Errorf("storage channel %q is not resolved yet (is the bot an admin of the channel?)", s.cfg.TelegramChannelID)
	}
	return s.channel, nil
}

func (s *Service) inputChannel() (tg.InputChannelClass, error) {
	peer, err := s.channelPeer()
	if err != nil {
		return nil, err
	}
	p, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return nil, fmt.Errorf("storage channel peer has unexpected type %T", peer)
	}
	return &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash}, nil
}

// resolveChannelLoop keeps retrying channel resolution until it succeeds.
func (s *Service) resolveChannelLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.channelResolved() {
			return
		}
		if err := s.resolveChannel(ctx); err != nil {
			slog.Warn("storage channel not resolved yet, retrying",
				"channel", s.cfg.TelegramChannelID, "err", err)
			timer.Reset(30 * time.Second)
			continue
		}
		return
	}
}

// resolveChannel resolves TELEGRAM_CHANNEL_ID (numeric id or @username) to
// an input peer.
func (s *Service) resolveChannel(ctx context.Context) error {
	s.mu.RLock()
	done := s.channel != nil
	api := s.api
	s.mu.RUnlock()
	if done || api == nil {
		return nil
	}

	raw := strings.TrimSpace(s.cfg.TelegramChannelID)
	if isUsername(raw) {
		domain := strings.TrimPrefix(raw, "@")
		res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: domain})
		if err != nil {
			return fmt.Errorf("resolve @%s: %w", domain, err)
		}
		pc, ok := res.Peer.(*tg.PeerChannel)
		if !ok {
			return fmt.Errorf("@%s does not resolve to a channel", domain)
		}
		for _, c := range res.Chats {
			ch, ok := c.(*tg.Channel)
			if ok && ch.ID == pc.ChannelID {
				s.setChannel(ch)
				return nil
			}
		}
		return fmt.Errorf("@%s resolved but channel info is missing", domain)
	}

	id, err := parseChannelID(raw)
	if err != nil {
		return err
	}
	res, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: id, AccessHash: 0},
	})
	if err != nil {
		return fmt.Errorf("channels.getChannels(%d): %w (make sure the bot is an admin of the channel)", id, err)
	}
	chats, ok := res.(*tg.MessagesChats)
	if !ok {
		return fmt.Errorf("channel %d was not returned by Telegram", id)
	}
	for _, c := range chats.Chats {
		if ch, ok := c.(*tg.Channel); ok && ch.ID == id {
			s.setChannel(ch)
			return nil
		}
	}
	return fmt.Errorf("channel %d was not returned by Telegram", id)
}

func (s *Service) channelResolved() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channel != nil
}

// captureChannelFromEntities is a fallback resolution path: any update that
// carries the storage channel (e.g. somebody posts in it) reveals its access
// hash.
func (s *Service) captureChannelFromEntities(entities tg.Entities) {
	if s.channelResolved() {
		return
	}
	raw := strings.TrimSpace(s.cfg.TelegramChannelID)
	if isUsername(raw) {
		return
	}
	id, err := parseChannelID(raw)
	if err != nil {
		return
	}
	if ch, ok := entities.Channels[id]; ok && ch.AccessHash != 0 {
		s.setChannel(ch)
	}
}

func (s *Service) setChannel(ch *tg.Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channel = &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
	slog.Info("storage channel resolved", "id", ch.ID, "title", ch.Title)
}

func isUsername(s string) bool {
	if strings.HasPrefix(s, "@") {
		return true
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err != nil
}

// parseChannelID accepts 1234567890, -1234567890 or -1001234567890.
func parseChannelID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("TELEGRAM_CHANNEL_ID %q is neither a numeric id nor an @username", s)
	}
	if n < 0 {
		n = -n
	}
	const prefix = int64(1_000_000_000_000) // the "-100" prefix used by clients
	if n >= prefix {
		n -= prefix
	}
	return n, nil
}
