package compress

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash"
	"math/big"

	"github.com/icza/bitio"
)

// Stream is an inefficient data structure used for easy experimentation with compression algorithms.
type Stream struct {
	D       []int
	NbSymbs int
}

func (s *Stream) Len() int {
	return len(s.D)
}

func (s *Stream) RunLen(i int) int {
	runLen := 1
	for i+runLen < len(s.D) && s.D[i+runLen] == 0 {
		runLen++
	}
	return runLen
}

func (s *Stream) At(i int) int {
	return s.D[i]
}

func NewStream(in []byte, bitsPerSymbol uint8) (Stream, error) {
	d := make([]int, len(in)*8/int(bitsPerSymbol))
	r := bitio.NewReader(bytes.NewReader(in))
	for i := range d {
		if n, err := r.ReadBits(bitsPerSymbol); err != nil {
			return Stream{}, err
		} else {
			d[i] = int(n)
		}
	}
	return Stream{d, 1 << int(bitsPerSymbol)}, nil
}

func (s *Stream) BreakUp(nbSymbs int) Stream {
	newPerOld := log(s.NbSymbs, nbSymbs)
	d := make([]int, len(s.D)*newPerOld)

	for i := range s.D {
		v := s.D[i]
		for j := 0; j < newPerOld; j++ {
			d[(i+1)*newPerOld-j-1] = v % nbSymbs
			v /= nbSymbs
		}
	}

	return Stream{d, nbSymbs}
}

// todo @tabaie too many copy pastes in the next three funcs

func (s *Stream) Pack(nbBits int) []*big.Int {
	wordLen := bitLen(s.NbSymbs)
	wordsPerElem := (nbBits - 1) / wordLen

	var radix big.Int
	radix.Lsh(big.NewInt(1), uint(wordLen))

	packed := make([]*big.Int, (len(s.D)+wordsPerElem-1)/wordsPerElem)
	for i := range packed {
		packed[i] = new(big.Int)
		for j := wordsPerElem - 1; j >= 0; j-- {
			absJ := i*wordsPerElem + j
			if absJ >= len(s.D) {
				continue
			}
			packed[i].Mul(packed[i], &radix).Add(packed[i], big.NewInt(int64(s.D[absJ])))
		}
	}
	return packed
}

// FillBytes aligns the stream first according to "field elements" of length nbBits, and then aligns the field elements to bytes
func (s *Stream) FillBytes(dst []byte, nbBits int) error {
	bitsPerWord := bitLen(s.NbSymbs)

	if bitsPerWord >= nbBits {
		return errors.New("words do not fit in elements")
	}

	wordsPerElem := (nbBits - 1) / bitsPerWord
	bytesPerElem := (nbBits + 7) / 8

	nbElems := (len(s.D) + wordsPerElem - 1) / wordsPerElem

	if len(dst) < (len(s.D)*bitsPerWord+7)/8+4 {
		return errors.New("not enough room in dst")
	}

	binary.BigEndian.PutUint32(dst[:4], uint32(len(s.D)))
	dst = dst[4:]

	var radix, elem big.Int // todo @tabaie all this big.Int business seems unnecessary. try using bitio instead?
	radix.Lsh(big.NewInt(1), uint(bitsPerWord))

	for i := 0; i < nbElems; i++ {
		elem.SetInt64(0)
		for j := 0; j < wordsPerElem; j++ {
			absJ := i*wordsPerElem + j
			if absJ >= len(s.D) {
				break
			}
			elem.Mul(&elem, &radix).Add(&elem, big.NewInt(int64(s.D[absJ])))
		}
		elem.FillBytes(dst[i*bytesPerElem : (i+1)*bytesPerElem])
	}
	return nil
}

// ReadBytes first reads elements of length nbBits in a byte-aligned manner, and then reads the elements into the stream
func (s *Stream) ReadBytes(src []byte, nbBits int) error {
	bitsPerWord := bitLen(s.NbSymbs)

	if bitsPerWord >= nbBits {
		return errors.New("words do not fit in elements")
	}

	if s.NbSymbs != 1<<bitsPerWord {
		return errors.New("only powers of 2 currently supported for NbSymbs")
	}

	s.resize(int(binary.BigEndian.Uint32(src[:4])))
	src = src[4:]

	wordsPerElem := (nbBits - 1) / bitsPerWord
	bytesPerElem := (nbBits + 7) / 8
	nbElems := (len(s.D) + wordsPerElem - 1) / wordsPerElem

	if len(src) < nbElems*bytesPerElem {
		return errors.New("not enough bytes")
	}

	w := bitio.NewReader(bytes.NewReader(src))

	for i := 0; i < nbElems; i++ {
		w.TryReadBits(uint8(8*bytesPerElem - bitsPerWord*wordsPerElem))
		if i+1 == nbElems {
			wordsToRead := len(s.D) - i*wordsPerElem
			w.TryReadBits(uint8((wordsPerElem - wordsToRead) * bitsPerWord)) // skip unused bits
		}
		for j := 0; j < wordsPerElem; j++ {
			wordI := i*wordsPerElem + j
			if wordI >= len(s.D) {
				continue
			}
			s.D[wordI] = int(w.TryReadBits(uint8(bitsPerWord)))
		}
	}

	return w.TryError
}

func (s *Stream) resize(_len int) {
	if len(s.D) < _len {
		s.D = make([]int, _len)
	}
	s.D = s.D[:_len]
}

func log(x, base int) int {
	exp := 0
	for pow := 1; pow < x; pow *= base {
		exp++
	}
	return exp
}

func (s *Stream) Checksum(hsh hash.Hash, fieldBits int) []byte {
	packed := s.Pack(fieldBits)
	fieldBytes := (fieldBits + 7) / 8
	byts := make([]byte, fieldBytes)
	for _, w := range packed {
		w.FillBytes(byts)
		hsh.Write(byts)
	}

	length := make([]byte, fieldBytes)
	big.NewInt(int64(s.Len())).FillBytes(length)
	hsh.Write(length)

	return hsh.Sum(nil)
}

func (s *Stream) WriteNum(r int, nbWords int) *Stream {
	for i := 0; i < nbWords; i++ {
		s.D = append(s.D, r%s.NbSymbs)
		r /= s.NbSymbs
	}
	if r != 0 {
		panic("overflow")
	}
	return s
}

func (s *Stream) ReadNum(start, nbWords int) int {
	res := 0
	for j := nbWords - 1; j >= 0; j-- {
		res *= s.NbSymbs
		res += s.D[start+j]
	}
	return res
}

func bitLen(n int) int {
	bitLen := 0
	for 1<<bitLen < n {
		bitLen++
	}
	return bitLen
}

// ToBytes writes the CONTENT of the stream to a byte slice, with no metadata about the size of the stream or the number of symbols.
// it mainly serves testing purposes so in case of a write error it panics.
func (s *Stream) ToBytes() []byte {
	bitsPerWord := bitLen(s.NbSymbs)

	nbBytes := (len(s.D)*bitsPerWord + 7) / 8
	bb := bytes.NewBuffer(make([]byte, 0, nbBytes))

	w := bitio.NewWriter(bb)
	for i := range s.D {
		w.TryWriteBits(uint64(s.D[i]), uint8(bitsPerWord))
	}
	if w.TryError != nil {
		panic(w.TryError)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	return bb.Bytes()
}
