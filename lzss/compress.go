package lzss

import (
	"bytes"
	"fmt"
	"math/bits"

	"github.com/consensys/compress/lzss/internal/suffixarray"
	"github.com/icza/bitio"
)

type Compressor struct {
	buf bytes.Buffer
	bw  *bitio.Writer

	inputIndex *suffixarray.Index
	inputSa    [MaxInputSize]int32 // suffix array space.

	dictData  []byte
	dictIndex *suffixarray.Index
	dictSa    [MaxDictSize]int32 // suffix array space.

	level Level
}

type Level uint8

const (
	NoCompression Level = 0
	// BestCompression allows the compressor to produce a stream of bit-level granularity,
	// giving the compressor this freedom helps it achieve better compression ratios but
	// will impose a high number of constraints on the SNARK decompressor
	BestCompression Level = 1

	GoodCompression        Level = 2
	GoodSnarkDecompression Level = 4

	// BestSnarkDecompression forces the compressor to produce byte-aligned output.
	// It is convenient and efficient for the SNARK decompressor but can hurt the compression ratio significantly
	BestSnarkDecompression Level = 8
)

const (
	bitLen = 16 // TODO @Tabaie document
)

// NewCompressor returns a new compressor with the given dictionary
func NewCompressor(dict []byte, level Level) (*Compressor, error) {
	dict = AugmentDict(dict)
	if len(dict) > MaxDictSize {
		return nil, fmt.Errorf("dict size must be <= %d", MaxDictSize)
	}
	c := &Compressor{
		dictData: dict,
	}
	c.buf.Grow(MaxInputSize)
	if level != NoCompression {
		// if we don't compress we don't need the dict.
		c.dictIndex = suffixarray.New(c.dictData, c.dictSa[:len(c.dictData)])
	}
	c.level = level
	return c, nil
}

// AugmentDict ensures the dictionary contains the special symbols
func AugmentDict(dict []byte) []byte {

	found := uint8(0)
	const mask uint8 = 0b111
	for _, b := range dict {
		if b == SymbolDict {
			found |= 0b001
		} else if b == SymbolShort {
			found |= 0b010
		} else if b == SymbolLong {
			found |= 0b100
		} else {
			continue
		}
		if found == mask {
			return dict
		}
	}

	return append(dict, SymbolDict, SymbolShort, SymbolLong)
}

func InitBackRefTypes(dictLen int, level Level) (short, long, dict BackrefType) {
	wordAlign := func(a int) uint8 {
		return (uint8(a) + uint8(level) - 1) / uint8(level) * uint8(level)
	}
	if level == NoCompression {
		wordAlign = func(a int) uint8 {
			return uint8(a)
		}
	}
	short = newBackRefType(SymbolShort, wordAlign(14), 8, false)
	long = newBackRefType(SymbolLong, wordAlign(19), 8, false)
	dict = newBackRefType(SymbolDict, wordAlign(bits.Len(uint(dictLen))), 8, true)
	return
}

// Compress compresses the given data; if hint is provided, the compressor will try to use it
// hint should be a subset of the data compressed by the same compressor
// For example, calling Compress([]byte{1, 2, 3, 4, 5}, compressed([]byte{1, 2, 3})) will
// result in much faster compression than calling Compress([]byte{1, 2, 3, 4, 5})
func (compressor *Compressor) Compress(input []byte, hints ...[]byte) (c []byte, err error) {
	// check input size
	if len(input) > MaxInputSize {
		return nil, fmt.Errorf("input size must be <= %d", MaxInputSize)
	}

	// reset output buffer
	compressor.buf.Reset()

	// write header
	header := Header{Version: Version, Level: compressor.level}
	if _, err = header.WriteTo(&compressor.buf); err != nil {
		return
	}

	// write uncompressed data if compression is disabled
	if compressor.level == NoCompression {
		compressor.buf.Write(input)
		return compressor.buf.Bytes(), nil
	}

	// initialize bit writer & backref types
	compressor.bw = bitio.NewWriter(&compressor.buf)
	shortBackRefType, longBackRefType, dictBackRefType := InitBackRefTypes(len(compressor.dictData), compressor.level)

	startI := 0
	if len(hints) == 1 {
		// try to compress from the hint to save time (no need to look for optimal backrefs
		// if we already have a good enough hint)
		startI = compressor.compressFromHint(header, input, hints[0])
	}

	// build the index
	compressor.inputIndex = suffixarray.New(input, compressor.inputSa[:len(input)])

	bDict := backref{bType: dictBackRefType, length: -1, address: -1}
	bShort := backref{bType: shortBackRefType, length: -1, address: -1}
	bLong := backref{bType: longBackRefType, length: -1, address: -1}

	fillBackrefs := func(i int, minLen int) bool {
		bDict.address, bDict.length = compressor.findBackRef(input, i, dictBackRefType, minLen)
		bShort.address, bShort.length = compressor.findBackRef(input, i, shortBackRefType, minLen)
		bLong.address, bLong.length = compressor.findBackRef(input, i, longBackRefType, minLen)
		return !(bDict.length == -1 && bShort.length == -1 && bLong.length == -1)
	}
	bestBackref := func() (backref, int) {
		if bDict.length != -1 && bDict.savings() > bShort.savings() && bDict.savings() > bLong.savings() {
			return bDict, bDict.savings()
		}
		if bShort.length != -1 && bShort.savings() > bLong.savings() {
			return bShort, bShort.savings()
		}
		return bLong, bLong.savings()
	}

	for i := startI; i < len(input); {
		if !canEncodeSymbol(input[i]) {
			// we must find a backref.
			if !fillBackrefs(i, 1) {
				// we didn't find a backref but can't write the symbol directly
				return nil, fmt.Errorf("could not find a backref at index %d", i)
			}
			best, _ := bestBackref()
			best.writeTo(compressor.bw, i)
			i += best.length
			continue
		}
		if !fillBackrefs(i, -1) {
			// we didn't find a backref, let's write the symbol directly
			compressor.writeByte(input[i])
			i++
			continue
		}
		bestAtI, bestSavings := bestBackref()

		if i+1 < len(input) {
			if fillBackrefs(i+1, bestAtI.length+1) {
				if newBest, newSavings := bestBackref(); newSavings > bestSavings {
					// we found a better backref at i+1
					compressor.writeByte(input[i])
					i++

					// then mark backref to be written at i+1
					bestSavings = newSavings
					bestAtI = newBest

					// can we find an even better backref at i+2 ?
					if canEncodeSymbol(input[i]) && i+1 < len(input) {
						if fillBackrefs(i+1, bestAtI.length+1) {
							// we found an even better backref
							if newBest, newSavings := bestBackref(); newSavings > bestSavings {
								compressor.writeByte(input[i])
								i++

								bestAtI = newBest
							}
						}
					}
				}
			} else if i+2 < len(input) && canEncodeSymbol(input[i+1]) {
				// maybe at i+2 ? (we already tried i+1)
				if fillBackrefs(i+2, bestAtI.length+2) {
					if newBest, newSavings := bestBackref(); newSavings > bestSavings {
						// we found a better backref
						// write the symbol at i
						compressor.writeByte(input[i])
						i++
						compressor.writeByte(input[i])
						i++

						// then emit the backref at i+2
						bestAtI = newBest
					}
				}
			}
		}

		bestAtI.writeTo(compressor.bw, i)
		i += bestAtI.length
	}

	if compressor.bw.TryError != nil {
		return nil, compressor.bw.TryError
	}
	if err = compressor.bw.Close(); err != nil {
		return nil, err
	}

	if compressor.buf.Len() >= len(input)+bitLen/8 {
		// compression was not worth it
		compressor.buf.Reset()
		header.Level = NoCompression
		if _, err = header.WriteTo(&compressor.buf); err != nil {
			return
		}
		_, err = compressor.buf.Write(input)
	}

	return compressor.buf.Bytes(), err
}

// compressFromHint attempts to compress the data using the hint
// and returns the number of bytes written to the buffer
// it essentially runs the decompress algorithm and checks that the backrefs are usable.
func (compressor *Compressor) compressFromHint(header Header, input, hint []byte) (startI int) {
	shortBackRefType, longBackRefType, dictBackRefType := InitBackRefTypes(len(compressor.dictData), compressor.level)

	bDict := backref{bType: dictBackRefType}
	bShort := backref{bType: shortBackRefType}
	bLong := backref{bType: longBackRefType}

	in := bitio.NewReader(bytes.NewReader(hint))

	var hintHeader Header
	if _, err := hintHeader.ReadFrom(in); err != nil {
		return
	}
	if hintHeader.Version != header.Version || hintHeader.Level != header.Level {
		// hint is not usable.
		return
	}
	if hintHeader.Level == NoCompression {
		return
	}

	// read byte per byte; if it's a backref, write the corresponding bytes
	// otherwise, write the byte as is
	s := in.TryReadByte()
	var out bytes.Buffer
	out.Grow(len(input))
	for in.TryError == nil {
		switch s {
		case SymbolShort:
			// short back ref
			bShort.readFrom(in)
			nad := out.Len() - bShort.address
			for i := 0; i < bShort.length; i++ {
				out.WriteByte(out.Bytes()[out.Len()-bShort.address])
			}
			decompressed := out.Bytes()[startI : startI+bShort.length]
			if !bytes.Equal(decompressed, input[startI:startI+bShort.length]) {
				// this is not a good backref; escape.
				return
			}
			// emit the backref
			bShort.address = nad
			bShort.writeTo(compressor.bw, startI)
			startI += bShort.length
		case SymbolLong:
			// long back ref
			bLong.readFrom(in)
			nad := out.Len() - bLong.address
			for i := 0; i < bLong.length; i++ {
				out.WriteByte(out.Bytes()[out.Len()-bLong.address])
			}
			// compare the last bLong.length bytes of out with d
			decompressed := out.Bytes()[startI : startI+bLong.length]
			if !bytes.Equal(decompressed, input[startI:startI+bLong.length]) {
				// this is not a good backref; escape.
				return
			}
			// emit the backref
			bLong.address = nad

			bLong.writeTo(compressor.bw, startI)
			startI += bLong.length
		case SymbolDict:
			// dict back ref
			bDict.readFrom(in)
			// compare the dict slice with d at the same position
			if !bytes.Equal(compressor.dictData[bDict.address:bDict.address+bDict.length], input[startI:startI+bDict.length]) {
				// this is not a good backref; escape.
				return
			}
			// emit the backref
			bDict.writeTo(compressor.bw, startI)
			startI += bDict.length

			// write on out for future refs.
			out.Write(compressor.dictData[bDict.address : bDict.address+bDict.length])

		default:
			if s != input[startI] {
				return
			}
			compressor.writeByte(input[startI])
			startI++
			out.WriteByte(s)
		}
		s = in.TryReadByte()
	}

	return
}

// canEncodeSymbol returns true if the symbol can be encoded directly
func canEncodeSymbol(b byte) bool {
	return b != SymbolDict && b != SymbolShort && b != SymbolLong
}

func (compressor *Compressor) writeByte(b byte) {
	if !canEncodeSymbol(b) {
		panic("cannot encode symbol")
	}
	compressor.bw.TryWriteByte(b)
}

// findBackRef attempts to find a backref in the window [i-brAddressRange, i+brLengthRange]
// if no backref is found, it returns -1, -1
// else returns the address and length of the backref
func (compressor *Compressor) findBackRef(data []byte, i int, bType BackrefType, minLength int) (addr, length int) {
	if minLength == -1 {
		minLength = bType.nbBytesBackRef
	}

	if i+minLength > len(data) {
		return -1, -1
	}

	windowStart := max(0, i-bType.maxAddress)
	maxRefLen := bType.maxLength

	if i+maxRefLen > len(data) {
		maxRefLen = len(data) - i
	}

	if minLength > maxRefLen {
		return -1, -1
	}

	if bType.dictOnly {
		return compressor.dictIndex.LookupLongest(data[i:i+maxRefLen], minLength, maxRefLen, 0, len(compressor.dictData))
	}

	return compressor.inputIndex.LookupLongest(data[i:i+maxRefLen], minLength, maxRefLen, windowStart, i)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
