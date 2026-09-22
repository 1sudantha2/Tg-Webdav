package telegram

import (
	"context"
	"fmt"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/1sudantha2/Tg-Webdav/internal/db"
	"github.com/1sudantha2/Tg-Webdav/internal/stream"
)

// maxPartSize is the maximum number of bytes a single upload.getFile call may
// request (Telegram hard limit: 1 MiB).
const maxPartSize = 1 << 20

// encodeDoc serializes a tg.Document for storage in the metadata index.
func encodeDoc(doc *tg.Document) ([]byte, error) {
	var b bin.Buffer
	if err := doc.Encode(&b); err != nil {
		return nil, err
	}
	return b.Buf, nil
}

// decodeDoc deserializes a stored tg.Document.
func decodeDoc(blob []byte) (*tg.Document, error) {
	var doc tg.Document
	b := bin.Buffer{Buf: blob}
	if err := doc.Decode(&b); err != nil {
		return nil, fmt.Errorf("decode document blob: %w", err)
	}
	return &doc, nil
}

// NewFileReader returns an io.ReadSeekCloser streaming the given indexed
// file straight from Telegram. Range requests and seeks translate into
// aligned upload.getFile calls; hot chunks are served from the shared cache.
func (s *Service) NewFileReader(ctx context.Context, node *db.Node) (io.ReadSeekCloser, error) {
	if node.IsDir {
		return nil, fmt.Errorf("telegram: %q is a directory", node.Name)
	}
	fetch := func(ctx context.Context, offset int64, size int) ([]byte, error) {
		return s.fetchChunk(ctx, node.DocID, offset, size)
	}
	return readSeekCloser{stream.NewReader(ctx, s.cache, docCacheKey(node.DocID), node.Size, fetch)}, nil
}

func docCacheKey(docID int64) string { return fmt.Sprintf("doc-%d", docID) }

type readSeekCloser struct{ *stream.Reader }

func (readSeekCloser) Close() error { return nil }

// fetchChunk reads one cache chunk (<= 4 MiB) of the indexed document,
// transparently refreshing expired file references.
func (s *Service) fetchChunk(ctx context.Context, docID int64, offset int64, size int) ([]byte, error) {
	node, err := s.store.NodeByDocID(ctx, docID)
	if err != nil {
		return nil, err
	}
	doc, err := decodeDoc(node.Doc)
	if err != nil {
		return nil, err
	}

	data, err := s.getFileRange(ctx, docInputLocation(doc), offset, size)
	if err != nil && tg.IsFileReferenceExpired(err) {
		fresh, rerr := s.refreshDocument(ctx, node)
		if rerr != nil {
			return nil, fmt.Errorf("fetch: %w (and refresh failed: %v)", err, rerr)
		}
		data, err = s.getFileRange(ctx, docInputLocation(fresh), offset, size)
	}
	return data, err
}

func docInputLocation(doc *tg.Document) tg.InputFileLocationClass {
	return &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
		ThumbSize:     "",
	}
}

// getFileRange fetches [offset, offset+size) of the located file using one or
// more aligned upload.getFile requests of at most 1 MiB each.
func (s *Service) getFileRange(ctx context.Context, loc tg.InputFileLocationClass, offset int64, size int) ([]byte, error) {
	api, err := s.apiNow()
	if err != nil {
		return nil, err
	}

	buf := make([]byte, 0, size)
	for len(buf) < size {
		limit := size - len(buf)
		if limit > maxPartSize {
			limit = maxPartSize
		}
		resp, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Precise:  true,
			Location: loc,
			Offset:   offset + int64(len(buf)),
			Limit:    limit,
		})
		if err != nil {
			return nil, err
		}
		switch f := resp.(type) {
		case *tg.UploadFile:
			if len(f.Bytes) == 0 {
				if len(buf) == 0 {
					return nil, fmt.Errorf("telegram returned no data at offset %d", offset)
				}
				return buf, nil // reached end of file
			}
			buf = append(buf, f.Bytes...)
		case *tg.UploadFileCDNRedirect:
			return nil, fmt.Errorf("file lives on CDN DC %d which is not supported", f.DCID)
		default:
			return nil, fmt.Errorf("unexpected upload.getFile response %T", resp)
		}
	}
	return buf, nil
}

// refreshDocument re-fetches the channel message to obtain a fresh
// tg.Document (with a valid file reference) and updates the index.
func (s *Service) refreshDocument(ctx context.Context, node *db.Node) (*tg.Document, error) {
	api, err := s.apiNow()
	if err != nil {
		return nil, err
	}
	ch, err := s.inputChannel()
	if err != nil {
		return nil, err
	}
	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: ch,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: node.MsgID}},
	})
	if err != nil {
		return nil, err
	}
	var msgs []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesMessages:
		msgs = r.Messages
	case *tg.MessagesMessagesSlice:
		msgs = r.Messages
	case *tg.MessagesChannelMessages:
		msgs = r.Messages
	default:
		return nil, fmt.Errorf("unexpected messages result %T", res)
	}
	for _, m := range msgs {
		msg, ok := m.(*tg.Message)
		if !ok || msg.ID != node.MsgID {
			continue
		}
		mm, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok {
			return nil, fmt.Errorf("message %d no longer carries a document", node.MsgID)
		}
		doc, ok := mm.Document.(*tg.Document)
		if !ok {
			return nil, fmt.Errorf("message %d document unavailable", node.MsgID)
		}
		if blob, err := encodeDoc(doc); err == nil {
			_ = s.store.UpdateDoc(ctx, node.DocID, blob)
		}
		return doc, nil
	}
	return nil, fmt.Errorf("message %d not found in storage channel", node.MsgID)
}
