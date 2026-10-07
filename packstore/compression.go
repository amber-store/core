package packstore

import (
	"errors"
	"fmt"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/key"
)

// WithCompression sets the compression for the objects this store encodes:
// those given to Put, PutVerified and PutVerifiedDeferred, and those a batch
// carries as Data. The default is no compression. The setting belongs to
// this handle and is stored nowhere; reading never depends on it, and
// records that arrive already encoded keep the codec they have.
func WithCompression(c amberpack.Compression) Option {
	return func(cfg *config) { cfg.compression = c }
}

// CompressionFunc returns the compression to use for one object. def is the
// store's WithCompression value: returning it accepts the store's setting,
// returning the zero Compression stores the object raw, and anything else
// overrides the setting for this object.
//
// It runs on whichever goroutine is writing, several at once under
// WriteParallel, so it must be safe for concurrent use. It must not modify
// or keep data, and it must not call the store: a repair calls it with the
// store's append lock held. A returned value that does not validate fails
// that object's write with an error wrapping amberpack.ErrInvalidCompression.
type CompressionFunc func(k key.Key, data []byte, def amberpack.Compression) amberpack.Compression

// WithCompressionFor makes the store ask f for every object it encodes,
// whatever the WithCompression value is. f is not asked for an object the
// store already holds, nor for a pre-encoded record. A nil f means no
// callback.
func WithCompressionFor(f CompressionFunc) Option {
	return func(cfg *config) { cfg.compressionFor = f }
}

// encode returns the record to append for (k, data), compressed as this
// store's options say.
func (s *Store) encode(k key.Key, data []byte) ([]byte, error) {
	c := s.cfg.compression
	if s.cfg.compressionFor != nil {
		c = s.cfg.compressionFor(k, data, c)
	}
	rec, err := amberpack.EncodeRecordWith(k, data, c)
	if errors.Is(err, amberpack.ErrInvalidCompression) {
		return nil, fmt.Errorf("packstore: object %s: %w", k, err)
	}
	return rec, err
}
