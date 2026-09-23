package archivestore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Sealed archives are encrypted at rest so that neither a leaked bucket nor a
// copied archive-store volume discloses an agent's disk. The format is a
// fixed header followed by independently authenticated AES-256-GCM chunks, so
// any plaintext range can be read without decrypting what precedes it:
//
//	header  "KYBSEAL1"
//	chunk i ciphertext(plaintext[i*ChunkSize : (i+1)*ChunkSize]) || tag
//
// Each chunk's nonce is its index; its additional data is the index plus a
// final-chunk flag, so reordering, truncation and extension all fail
// authentication. The key is random per archive, which is what makes an
// index nonce safe. At least one (possibly empty) final chunk is always
// written.
const (
	ChunkSize  = 64 << 10
	sealHeader = "KYBSEAL1"
	tagSize    = 16
	sealedSize = ChunkSize + tagSize
	// KeySize is the length of a per-archive data key.
	KeySize = 32
)

var ErrSealCorrupt = errors.New("archivestore: sealed archive failed authentication")

// NewDataKey returns a fresh random per-archive key.
func NewDataKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("generating data key: %w", err)
	}
	return k, nil
}

// SealedLength is the stored size of a plaintext of n bytes.
func SealedLength(n int64) int64 {
	chunks := n/ChunkSize + 1
	if n > 0 && n%ChunkSize == 0 {
		chunks = n / ChunkSize
	}
	return int64(len(sealHeader)) + n + chunks*tagSize
}

// PlainLength inverts SealedLength, reporting false when no plaintext length
// seals to exactly n bytes.
func PlainLength(n int64) (int64, bool) {
	body := n - int64(len(sealHeader))
	if body < tagSize {
		return 0, false
	}
	full := body / sealedSize
	rem := body % sealedSize
	var plain int64
	switch {
	case rem == 0:
		plain = full * ChunkSize
	case rem >= tagSize:
		plain = full*ChunkSize + rem - tagSize
	default:
		return 0, false
	}
	return plain, SealedLength(plain) == n
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("archivestore: data key must be %d bytes", KeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(aead cipher.AEAD, i uint64) []byte {
	n := make([]byte, aead.NonceSize())
	binary.BigEndian.PutUint64(n[len(n)-8:], i)
	return n
}

func chunkAD(i uint64, final bool) []byte {
	ad := make([]byte, 9)
	binary.BigEndian.PutUint64(ad, i)
	if final {
		ad[8] = 1
	}
	return ad
}

// SealWriter encrypts everything written to it into w. Close must be called
// to emit the final chunk; an archive without it fails to open.
type SealWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	buf    []byte
	index  uint64
	plain  int64
	closed bool
	err    error
}

// NewSealWriter starts a sealed stream on w.
func NewSealWriter(w io.Writer, key []byte) (*SealWriter, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(w, sealHeader); err != nil {
		return nil, err
	}
	return &SealWriter{w: w, aead: aead, buf: make([]byte, 0, ChunkSize)}, nil
}

func (s *SealWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.closed {
		return 0, errors.New("archivestore: write after close")
	}
	written := 0
	for len(p) > 0 {
		// A full buffer is flushed only once more data arrives, so the last
		// chunk is always the one Close seals as final.
		if len(s.buf) == ChunkSize {
			if err := s.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(s.buf[len(s.buf):ChunkSize], p)
		s.buf = s.buf[:len(s.buf)+n]
		p = p[n:]
		written += n
		s.plain += int64(n)
	}
	return written, nil
}

func (s *SealWriter) flush(final bool) error {
	out := s.aead.Seal(nil, chunkNonce(s.aead, s.index), s.buf, chunkAD(s.index, final))
	if _, err := s.w.Write(out); err != nil {
		s.err = err
		return err
	}
	s.index++
	s.buf = s.buf[:0]
	return nil
}

// Close seals the final chunk. It does not close the underlying writer.
func (s *SealWriter) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.err != nil {
		return s.err
	}
	return s.flush(true)
}

// PlainBytes is the number of plaintext bytes written so far.
func (s *SealWriter) PlainBytes() int64 { return s.plain }

// RangeOpener reads a byte range of a stored object.
type RangeOpener func(offset, length int64) (io.ReadCloser, error)

// SealedReader is an io.ReaderAt over a sealed object's plaintext. It fetches
// a window of chunks per store read, so sequential consumers such as
// archive/zip make one ranged request per window rather than per chunk.
type SealedReader struct {
	open      RangeOpener
	aead      cipher.AEAD
	size      int64
	sealed    int64
	lastChunk uint64
	window    int
	cacheBase uint64
	cache     [][]byte
}

// NewSealedReader opens a sealed object of sealedSize stored bytes.
func NewSealedReader(open RangeOpener, sealedSize int64, key []byte) (*SealedReader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plain, ok := PlainLength(sealedSize)
	if !ok {
		return nil, fmt.Errorf("%w: impossible sealed length %d", ErrSealCorrupt, sealedSize)
	}
	last := uint64(plain / ChunkSize)
	if plain > 0 && plain%ChunkSize == 0 {
		last--
	}
	r := &SealedReader{open: open, aead: aead, size: plain, sealed: sealedSize, lastChunk: last, window: 64}
	// Check the header and the final chunk up front: a truncated or foreign
	// object fails here rather than midway through a consumer's read.
	rc, err := open(0, int64(len(sealHeader)))
	if err != nil {
		return nil, err
	}
	hdr, err := io.ReadAll(io.LimitReader(rc, int64(len(sealHeader))))
	rc.Close()
	if err != nil {
		return nil, err
	}
	if string(hdr) != sealHeader {
		return nil, fmt.Errorf("%w: missing header", ErrSealCorrupt)
	}
	if _, err := r.chunk(last); err != nil {
		return nil, err
	}
	return r, nil
}

// Size is the plaintext length.
func (r *SealedReader) Size() int64 { return r.size }

func (r *SealedReader) chunk(i uint64) ([]byte, error) {
	if r.cache != nil && i >= r.cacheBase && i < r.cacheBase+uint64(len(r.cache)) {
		return r.cache[i-r.cacheBase], nil
	}
	n := uint64(r.window)
	if i+n > r.lastChunk+1 {
		n = r.lastChunk + 1 - i
	}
	start := int64(len(sealHeader)) + int64(i)*sealedSize
	length := int64(n) * sealedSize
	if start+length > r.sealed {
		length = r.sealed - start
	}
	rc, err := r.open(start, length)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw := make([]byte, length)
	if _, err := io.ReadFull(rc, raw); err != nil {
		return nil, fmt.Errorf("reading sealed chunks: %w", err)
	}
	chunks := make([][]byte, 0, n)
	for j := uint64(0); j < n; j++ {
		idx := i + j
		end := int((j + 1) * sealedSize)
		if end > len(raw) {
			end = len(raw)
		}
		ct := raw[int(j*sealedSize):end]
		pt, err := r.aead.Open(nil, chunkNonce(r.aead, idx), ct, chunkAD(idx, idx == r.lastChunk))
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d", ErrSealCorrupt, idx)
		}
		chunks = append(chunks, pt)
	}
	r.cacheBase, r.cache = i, chunks
	return chunks[0], nil
}

// ReadAt implements io.ReaderAt over the plaintext.
func (r *SealedReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("archivestore: negative offset")
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		c, err := r.chunk(uint64(pos / ChunkSize))
		if err != nil {
			return n, err
		}
		n += copy(p[n:], c[pos%ChunkSize:])
	}
	return n, nil
}

// WrapKey encrypts a data key under a key-encryption key derived from secret
// (the installation's internal signing key), for storage beside the job.
func WrapKey(secret, dataKey []byte) ([]byte, error) {
	aead, err := newAEAD(deriveKEK(secret))
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, dataKey, []byte("kyber-archive-data-key")), nil
}

// UnwrapKey reverses WrapKey.
func UnwrapKey(secret, wrapped []byte) ([]byte, error) {
	aead, err := newAEAD(deriveKEK(secret))
	if err != nil {
		return nil, err
	}
	if len(wrapped) < aead.NonceSize() {
		return nil, ErrSealCorrupt
	}
	key, err := aead.Open(nil, wrapped[:aead.NonceSize()], wrapped[aead.NonceSize():], []byte("kyber-archive-data-key"))
	if err != nil {
		return nil, fmt.Errorf("%w: data key", ErrSealCorrupt)
	}
	return key, nil
}

func deriveKEK(secret []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("kyber archive key-encryption key v1"))
	return m.Sum(nil)
}
