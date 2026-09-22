package telegram

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/1sudantha2/Tg-Webdav/internal/db"
)

// UploadStream streams data from r into the storage channel and indexes the
// resulting file at dirPath/name. size is the total byte count, or -1 when
// unknown. No data is ever written to disk. When a file with the same name
// already exists, a numbered copy ("name (1).ext") is created.
//
// The returned node and the full virtual path are suitable for replying to
// the caller.
func (s *Service) UploadStream(ctx context.Context, dirPath, name string, r io.Reader, size int64, mimeType string, attrs []tg.DocumentAttributeClass) (*db.Node, string, error) {
	return s.uploadStream(ctx, dirPath, name, r, size, mimeType, attrs, false)
}

// UploadStreamReplace behaves like UploadStream but overwrites an existing
// file with the same name (the old Telegram message is deleted once the new
// upload is safely indexed).
func (s *Service) UploadStreamReplace(ctx context.Context, dirPath, name string, r io.Reader, size int64, mimeType string, attrs []tg.DocumentAttributeClass) (*db.Node, string, error) {
	return s.uploadStream(ctx, dirPath, name, r, size, mimeType, attrs, true)
}

func (s *Service) uploadStream(ctx context.Context, dirPath, name string, r io.Reader, size int64, mimeType string, attrs []tg.DocumentAttributeClass, replace bool) (*db.Node, string, error) {
	api, err := s.apiNow()
	if err != nil {
		return nil, "", err
	}
	if _, err := s.channelPeer(); err != nil {
		return nil, "", err
	}

	name = sanitizeName(name)
	if name == "" {
		name = decideName("", "", mimeType, time.Now())
	}

	dir, err := s.store.MkdirAll(ctx, dirPath)
	if err != nil {
		return nil, "", fmt.Errorf("create target folder: %w", err)
	}

	// Overwrite handling: drop the old index entry before the upload and
	// restore it if the upload fails.
	var replaced *db.Node
	if replace {
		if old, oerr := s.store.Child(ctx, dir.ID, name); oerr == nil && !old.IsDir {
			if _, derr := s.store.DeleteRecursive(ctx, old.ID); derr != nil {
				return nil, "", derr
			}
			replaced = old
		}
	}
	unique, err := s.store.UniqueName(ctx, dir.ID, name)
	if err != nil {
		return nil, "", err
	}
	name = unique

	restoreReplaced := func() {
		if replaced == nil {
			return
		}
		if _, rerr := s.store.CreateFile(ctx, dir.ID, replaced.Name, replaced.Size, replaced.MIME,
			replaced.MsgID, replaced.DocID, replaced.Doc); rerr != nil {
			slog.Error("could not restore replaced index entry", "name", replaced.Name, "err", rerr)
		}
	}

	up := uploader.NewUpload(name, r, size)
	inputFile, err := uploader.NewUploader(api).WithThreads(2).Upload(ctx, up)
	if err != nil {
		restoreReplaced()
		return nil, "", fmt.Errorf("telegram upload: %w", err)
	}

	doc, msgID, err := s.sendToChannel(ctx, inputFile, name, mimeType, attrs)
	if err != nil {
		restoreReplaced()
		return nil, "", err
	}

	blob, err := encodeDoc(doc)
	if err != nil {
		restoreReplaced()
		return nil, "", err
	}
	node, err := s.store.CreateFile(ctx, dir.ID, name, doc.Size, mimeType, msgID, doc.ID, blob)
	if err != nil {
		restoreReplaced()
		return nil, "", fmt.Errorf("index file: %w", err)
	}

	if replaced != nil {
		// The new file is safely indexed; drop the replaced Telegram copy.
		go s.MaybeDeleteChannelMessages(context.WithoutCancel(ctx), []*db.Node{replaced})
	}

	resolvedDir, perr := s.nodePath(ctx, dir)
	if perr != nil {
		resolvedDir = "/" + dir.Name
	}
	full := resolvedDir + "/" + name
	slog.Info("stored file", "path", full, "size", doc.Size, "msg", msgID)
	return node, full, nil
}

// sendToChannel publishes an already uploaded file to the storage channel and
// returns the final tg.Document (with a fresh file reference) and message id.
func (s *Service) sendToChannel(ctx context.Context, inputFile tg.InputFileClass, name, mimeType string, attrs []tg.DocumentAttributeClass) (*tg.Document, int, error) {
	api, err := s.apiNow()
	if err != nil {
		return nil, 0, err
	}
	peer, err := s.channelPeer()
	if err != nil {
		return nil, 0, err
	}

	hasFilename := false
	for _, a := range attrs {
		if _, ok := a.(*tg.DocumentAttributeFilename); ok {
			hasFilename = true
		}
	}
	if !hasFilename {
		attrs = append([]tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}}, attrs...)
	}

	randomID := randInt64()
	resp, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    &tg.InputMediaUploadedDocument{File: inputFile, MimeType: mimeType, Attributes: attrs},
		Message:  name,
		RandomID: randomID,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("send to storage channel (is the bot an admin with posting rights?): %w", err)
	}

	msg, ok := extractSentMessage(resp, randomID)
	if !ok {
		return nil, 0, fmt.Errorf("telegram accepted the upload but the resulting message could not be parsed")
	}
	mm, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, 0, fmt.Errorf("stored message %d has no document media", msg.ID)
	}
	doc, ok := mm.Document.(*tg.Document)
	if !ok {
		return nil, 0, fmt.Errorf("stored message %d document unavailable", msg.ID)
	}
	return doc, msg.ID, nil
}

// extractSentMessage finds the message created for randomID inside the
// updates returned by messages.sendMedia.
func extractSentMessage(upd tg.UpdatesClass, randomID int64) (*tg.Message, bool) {
	var updates []tg.UpdateClass
	switch u := upd.(type) {
	case *tg.Updates:
		updates = u.Updates
	case *tg.UpdatesCombined:
		updates = u.Updates
	default:
		return nil, false
	}

	msgID := -1
	for _, up := range updates {
		if um, ok := up.(*tg.UpdateMessageID); ok && um.RandomID == randomID {
			msgID = um.ID
		}
	}
	for _, up := range updates {
		var m tg.MessageClass
		switch v := up.(type) {
		case *tg.UpdateNewChannelMessage:
			m = v.Message
		case *tg.UpdateNewMessage:
			m = v.Message
		default:
			continue
		}
		if msg, ok := m.(*tg.Message); ok && (msgID < 0 || msg.ID == msgID) {
			return msg, true
		}
	}
	return nil, false
}

// DeleteChannelMessage removes a message from the storage channel. Errors are
// logged but not returned: an orphaned channel message only costs Telegram
// storage, never correctness.
func (s *Service) DeleteChannelMessage(ctx context.Context, msgID int) {
	if msgID == 0 {
		return
	}
	api, err := s.apiNow()
	if err != nil {
		return
	}
	ch, err := s.inputChannel()
	if err != nil {
		return
	}
	if _, err := api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
		Channel: ch,
		ID:      []int{msgID},
	}); err != nil {
		slog.Warn("could not delete channel message", "msg", msgID, "err", err)
		return
	}
	slog.Info("deleted channel message", "msg", msgID)
}

// pathOf reconstructs the virtual path of the node with the given id.
func (s *Service) pathOf(ctx context.Context, id int64) (string, error) {
	n, err := s.store.LookupByID(ctx, id)
	if err != nil {
		return "", err
	}
	return s.nodePath(ctx, n)
}

func (s *Service) nodePath(ctx context.Context, n *db.Node) (string, error) {
	name := n.Name
	for parent := n.ParentID; parent != 0; {
		p, err := s.store.LookupByID(ctx, parent)
		if err != nil {
			return "", err
		}
		name = p.Name + "/" + name
		parent = p.ParentID
	}
	return "/" + name, nil
}

// MaybeDeleteChannelMessages deletes the channel messages of the given nodes
// unless another indexed copy still references the same Telegram document.
func (s *Service) MaybeDeleteChannelMessages(ctx context.Context, nodes []*db.Node) {
	for _, n := range nodes {
		if n.MsgID == 0 || n.DocID == 0 {
			continue
		}
		count, err := s.store.CountByDocID(ctx, n.DocID)
		if err != nil || count > 0 {
			continue
		}
		s.DeleteChannelMessage(ctx, n.MsgID)
	}
}

func randInt64() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}
	v := int64(binary.BigEndian.Uint64(b[:]) & 0x7fffffffffffffff)
	if v == 0 {
		v = 1
	}
	return v
}
