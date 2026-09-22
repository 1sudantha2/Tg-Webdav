package telegram

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/gotd/td/tg"

	"github.com/1sudantha2/Tg-Webdav/internal/stream"
)

const helpText = "📥 Send me any document, video, audio or photo and I will store it in your WebDAV drive."

// registerUpdateHandlers wires the bot inbox listener and the channel access
// hash capture fallback.
func (s *Service) registerUpdateHandlers() {
	s.dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage) error {
		// Heavy ingest work must not block the update loop.
		go s.handleIncomingMessage(ctx, entities, u)
		return nil
	})
	s.dispatcher.OnNewChannelMessage(func(ctx context.Context, entities tg.Entities, u *tg.UpdateNewChannelMessage) error {
		s.captureChannelFromEntities(entities)
		return nil
	})
}

// handleIncomingMessage implements the Bot Inbox Listener: media sent to the
// bot in a private chat is re-uploaded to the storage channel and indexed in
// the configured default folder.
func (s *Service) handleIncomingMessage(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage) {
	m, ok := u.Message.(*tg.Message)
	if !ok || m.Out {
		return
	}
	if _, ok := m.PeerID.(*tg.PeerUser); !ok {
		return // only private chats with the bot are ingested
	}

	log := slog.With("user", peerUserID(m.FromID), "msg", m.ID)

	switch mm := m.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := mm.Document.(*tg.Document)
		if !ok || doc == nil {
			return
		}
		s.ingestDocument(ctx, entities, u, m, doc, log)
	case *tg.MessageMediaPhoto:
		photo, ok := mm.Photo.(*tg.Photo)
		if !ok || photo == nil {
			return
		}
		s.ingestPhoto(ctx, entities, u, m, photo, log)
	default:
		s.reply(ctx, entities, u, helpText)
	}
}

// ingestDocument streams a document (video, audio, file, voice note, ...)
// from the source chat into the storage channel without touching disk.
func (s *Service) ingestDocument(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage, m *tg.Message, doc *tg.Document, log *slog.Logger) {
	if doc.Size > 2<<30 {
		s.reply(ctx, entities, u, "❌ Files larger than 2 GB are not supported by Telegram.")
		return
	}

	name := decideName(m.Message, docFileName(doc), doc.MimeType, time.Unix(int64(m.Date), 0))

	// Stream the source document through the shared cache (keyed by the
	// source document id so it never clashes with stored files).
	src := stream.NewReader(ctx, s.cache, "src-"+strconv.FormatInt(doc.ID, 10), doc.Size,
		func(ctx context.Context, offset int64, size int) ([]byte, error) {
			return s.getFileRange(ctx, docInputLocation(doc), offset, size)
		})

	// Preserve the original attributes (duration, video size, voice flag ...)
	// but force the chosen file name.
	attrs := make([]tg.DocumentAttributeClass, 0, len(doc.Attributes))
	for _, a := range doc.Attributes {
		if _, isName := a.(*tg.DocumentAttributeFilename); !isName {
			attrs = append(attrs, a)
		}
	}

	node, full, err := s.UploadStream(ctx, s.cfg.DefaultFolder, name, src, doc.Size, doc.MimeType, attrs)
	if err != nil {
		log.Error("ingest failed", "err", err)
		s.reply(ctx, entities, u, "❌ Could not store the file: "+shortErr(err))
		return
	}
	log.Info("ingested", "path", full, "size", node.Size)
	s.reply(ctx, entities, u, fmt.Sprintf("✅ Saved %s (%s)", full, humanSize(node.Size)))
}

// ingestPhoto downloads the largest photo rendition and stores it as a JPEG
// document in the storage channel.
func (s *Service) ingestPhoto(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage, m *tg.Message, photo *tg.Photo, log *slog.Logger) {
	sizeType, ok := bestPhotoSize(photo)
	if !ok {
		s.reply(ctx, entities, u, "❌ This photo has no downloadable rendition.")
		return
	}
	loc := &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     sizeType,
	}
	data, err := s.downloadLocation(ctx, loc)
	if err != nil {
		log.Error("photo download failed", "err", err)
		s.reply(ctx, entities, u, "❌ Could not download the photo: "+shortErr(err))
		return
	}

	name := decideName(m.Message, "", "image/jpeg", time.Unix(int64(m.Date), 0))
	node, full, err := s.UploadStream(ctx, s.cfg.DefaultFolder, name, bytes.NewReader(data), int64(len(data)), "image/jpeg", nil)
	if err != nil {
		log.Error("photo ingest failed", "err", err)
		s.reply(ctx, entities, u, "❌ Could not store the photo: "+shortErr(err))
		return
	}
	log.Info("ingested photo", "path", full, "size", node.Size)
	s.reply(ctx, entities, u, fmt.Sprintf("✅ Saved %s (%s)", full, humanSize(node.Size)))
}

// downloadLocation fetches the whole located file into memory (used for
// photos only, which are always small).
func (s *Service) downloadLocation(ctx context.Context, loc tg.InputFileLocationClass) ([]byte, error) {
	api, err := s.apiNow()
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		resp, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise:  true,
			Location: loc,
			Offset:   int64(len(out)),
			Limit:    maxPartSize,
		})
		if err != nil {
			return nil, err
		}
		f, ok := resp.(*tg.UploadFile)
		if !ok {
			return nil, fmt.Errorf("unexpected upload.getFile response %T", resp)
		}
		if len(f.Bytes) == 0 {
			return out, nil
		}
		out = append(out, f.Bytes...)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Service) reply(ctx context.Context, entities tg.Entities, u *tg.UpdateNewMessage, text string) {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender == nil {
		return
	}
	if _, err := sender.Reply(entities, u).Text(ctx, text); err != nil {
		slog.Warn("could not reply to user", "err", err)
	}
}

// docFileName extracts the original file name from document attributes.
func docFileName(doc *tg.Document) string {
	for _, a := range doc.Attributes {
		if fn, ok := a.(*tg.DocumentAttributeFilename); ok && fn.FileName != "" {
			return fn.FileName
		}
	}
	return ""
}

// bestPhotoSize picks the largest concrete rendition of a photo.
func bestPhotoSize(photo *tg.Photo) (string, bool) {
	best := ""
	bestPx := -1
	for _, sc := range photo.Sizes {
		switch sz := sc.(type) {
		case *tg.PhotoSize:
			if sz.Size <= 0 {
				continue
			}
			if px := sz.W * sz.H; px > bestPx {
				bestPx, best = px, sz.Type
			}
		case *tg.PhotoSizeProgressive:
			if len(sz.Sizes) == 0 {
				continue
			}
			if px := sz.W * sz.H; px > bestPx {
				bestPx, best = px, sz.Type
			}
		}
	}
	return best, best != ""
}

func peerUserID(p tg.PeerClass) int64 {
	if u, ok := p.(*tg.PeerUser); ok {
		return u.UserID
	}
	return 0
}

func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

