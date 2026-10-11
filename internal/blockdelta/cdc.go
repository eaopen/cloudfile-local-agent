// Package blockdelta builds Seafile-compatible CDC manifests from immutable
// local snapshots. Publication must go through CloudFile's guarded Editing Core.
package blockdelta

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"strings"
)

const (
	MinSize           = 6 << 20
	AvgSize           = 8 << 20
	MaxSize           = 10 << 20
	WindowSize        = 48
	MaxPlanSize int64 = 16 << 30
)
const rabinPoly uint64 = 0xbfe6b8a5bf378d83

type Block struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	FileID string  `json:"file_id"`
	SHA256 string  `json:"sha256"`
	Size   int64   `json:"size"`
	Blocks []Block `json:"blocks"`
}

type rabinTables struct {
	t     [256]uint64
	u     [256]uint64
	shift uint
}

func polyMod(hi, lo, divisor uint64) uint64 {
	k := bits.Len64(divisor) - 1
	d := divisor << uint(63-k)
	if hi&(uint64(1)<<63) != 0 {
		hi ^= d
	}
	for i := 62; i >= 0; i-- {
		if hi&(uint64(1)<<uint(i)) != 0 {
			hi ^= d >> uint(63-i)
			lo ^= d << uint(i+1)
		}
	}
	for i := 63; i >= k; i-- {
		if lo&(uint64(1)<<uint(i)) != 0 {
			lo ^= d >> uint(63-i)
		}
	}
	return lo
}
func polyMultiply(x, y uint64) (hi, lo uint64) {
	if x&1 != 0 {
		lo = y
	}
	for i := 1; i < 64; i++ {
		if x&(uint64(1)<<uint(i)) != 0 {
			hi ^= y >> uint(64-i)
			lo ^= y << uint(i)
		}
	}
	return hi, lo
}
func polyMultiplyMod(x, y uint64) uint64 { h, l := polyMultiply(x, y); return polyMod(h, l, rabinPoly) }
func newRabin() rabinTables {
	var r rabinTables
	shift := bits.Len64(rabinPoly) - 1
	r.shift = uint(shift - 8)
	t1 := polyMod(0, uint64(1)<<uint(shift), rabinPoly)
	for i := 0; i < 256; i++ {
		r.t[i] = polyMultiplyMod(uint64(i), t1) | uint64(i)<<uint(shift)
	}
	pow := uint64(1)
	for i := 1; i < WindowSize; i++ {
		pow = r.append(pow, 0)
	}
	for i := 0; i < 256; i++ {
		r.u[i] = polyMultiplyMod(uint64(i), pow)
	}
	return r
}
func (r *rabinTables) append(p uint64, b byte) uint64 { return (p<<8 | uint64(b)) ^ r.t[p>>r.shift] }
func (r *rabinTables) roll(p uint32, out, in byte) uint32 {
	return uint32(r.append(uint64(p)^r.u[out], in))
}
func (r *rabinTables) fingerprint(buf []byte) uint32 {
	var p uint32
	for _, b := range buf {
		p = r.roll(p, 0, b)
	}
	return p
}
func findCut(buf []byte, r *rabinTables) int {
	if len(buf) < MinSize {
		return len(buf)
	}
	var fp uint32
	for i := MinSize - 1; i < len(buf); i++ {
		if i == MinSize-1 {
			fp = r.fingerprint(buf[i-WindowSize+1 : i+1])
		} else {
			fp = r.roll(fp, buf[i-WindowSize], buf[i])
		}
		if fp&(AvgSize-1) == 0x13 || i+1 >= MaxSize {
			return i + 1
		}
	}
	return len(buf)
}

// ScanSnapshot reads only a frozen, regular input, never mutates it. This
// currently supports only non-encrypted, block-hash-enabled repo version 1+.
// In particular no directory sync or delta download is implemented.
func ScanSnapshot(path string, repoVersion int) (Manifest, error) {
	if repoVersion < 1 {
		return Manifest{}, errors.New("repo version 1+ required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, err
	}
	if !before.Mode().IsRegular() || before.Size() > MaxPlanSize {
		return Manifest{}, errors.New("bounded regular snapshot required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return Manifest{}, errors.New("snapshot identity changed")
	}
	m := Manifest{Size: before.Size()}
	r := newRabin()
	buf := make([]byte, MaxSize)
	h := sha256.New()
	reader := io.TeeReader(file, h)
	tail := 0
	var offset int64
	for offset < before.Size() || tail > 0 {
		n, readErr := io.ReadFull(reader, buf[tail:])
		tail += n
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return Manifest{}, readErr
		}
		if readErr != nil && offset+int64(tail) != before.Size() {
			return Manifest{}, errors.New("snapshot incomplete")
		}
		if tail == 0 {
			break
		}
		cut := findCut(buf[:tail], &r)
		if cut < 1 || cut > tail {
			return Manifest{}, errors.New("CDC boundary invalid")
		}
		sum := sha1.Sum(buf[:cut])
		m.Blocks = append(m.Blocks, Block{ID: hex.EncodeToString(sum[:]), Offset: offset, Size: int64(cut)})
		offset += int64(cut)
		copy(buf, buf[cut:tail])
		tail -= cut
		if readErr != nil && tail == 0 {
			break
		}
	}
	if offset != before.Size() {
		return Manifest{}, errors.New("snapshot size changed")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return Manifest{}, errors.New("snapshot modified during scan")
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	m.FileID = FileObjectID(m)
	return m, nil
}

// FileObjectID mimics the Seafile v1 file object JSON_SORT_KEYS encoding.
// The byte spacing is intentional: Jansson's canonical JSON has spaces.
func FileObjectID(m Manifest) string {
	ids := make([]string, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		ids = append(ids, fmt.Sprintf("%q", b.ID))
	}
	data := fmt.Sprintf(`{"block_ids": [%s], "size": %d, "type": 1, "version": 1}`, strings.Join(ids, ", "), m.Size)
	sum := sha1.Sum([]byte(data))
	return hex.EncodeToString(sum[:])
}
