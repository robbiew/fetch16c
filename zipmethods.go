package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
)

// Legacy PKZIP compression methods that archive/zip does not implement. Packs from the early
// 1990s were built with PKZIP 1.x, which used Shrink and Implode.
const (
	zipMethodShrink  = 1
	zipMethodImplode = 6
)

// Implode general purpose flags.
const (
	implodeFlag8K     = 0x0002 // 8K sliding dictionary instead of 4K
	implodeFlag3Trees = 0x0004 // literals are coded with a third Shannon-Fano tree
)

var errCorruptLegacyZip = errors.New("corrupt legacy ZIP data")

// bitReader reads bits least significant first, as PKZIP wrote them.
type bitReader struct {
	reader io.ByteReader
	bits   uint32
	count  uint
}

func newBitReader(reader io.Reader) *bitReader {
	if byteReader, ok := reader.(io.ByteReader); ok {
		return &bitReader{reader: byteReader}
	}
	return &bitReader{reader: &singleByteReader{reader: reader}}
}

type singleByteReader struct {
	reader io.Reader
	buffer [1]byte
}

func (reader *singleByteReader) ReadByte() (byte, error) {
	_, err := io.ReadFull(reader.reader, reader.buffer[:])
	return reader.buffer[0], err
}

func (reader *bitReader) readBits(count uint) (uint32, error) {
	for reader.count < count {
		next, err := reader.reader.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		reader.bits |= uint32(next) << reader.count
		reader.count += 8
	}
	value := reader.bits & (1<<count - 1)
	reader.bits >>= count
	reader.count -= count
	return value, nil
}

func (reader *bitReader) readBit() (uint32, error) {
	return reader.readBits(1)
}

// shannonFanoTree decodes the canonical codes PKZIP's Implode uses.
type shannonFanoTree struct {
	// codes maps (length << 16 | code) to a symbol.
	codes     map[uint32]uint16
	maxLength uint
}

// readShannonFanoTree parses the compact tree description: a byte holding the number of
// following bytes minus one, then bytes whose low nibble is a bit length minus one and whose
// high nibble is how many consecutive symbols, minus one, share that length.
func readShannonFanoTree(reader io.ByteReader, symbols int) (*shannonFanoTree, error) {
	countByte, err := reader.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("%w: truncated tree", errCorruptLegacyZip)
	}
	lengths := make([]uint, 0, symbols)
	for range int(countByte) + 1 {
		entry, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: truncated tree", errCorruptLegacyZip)
		}
		length := uint(entry&0x0F) + 1
		repeat := int(entry>>4) + 1
		for range repeat {
			lengths = append(lengths, length)
		}
	}
	if len(lengths) != symbols {
		return nil, fmt.Errorf("%w: tree describes %d symbols, want %d", errCorruptLegacyZip, len(lengths), symbols)
	}

	// PKZIP assigns canonical codes in order of decreasing code length and, within one length,
	// decreasing symbol value, so the longest codes get the smallest values. Each code's first
	// bit in the stream is its most significant bit.
	tree := &shannonFanoTree{codes: make(map[uint32]uint16, symbols)}
	order := make([]int, symbols)
	for symbol := range symbols {
		order[symbol] = symbol
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if lengths[a] != lengths[b] {
			return lengths[a] > lengths[b]
		}
		return a > b
	})
	code := uint32(0)
	increment := uint32(0)
	lastLength := uint(0)
	for _, symbol := range order {
		code += increment
		length := lengths[symbol]
		if length != lastLength {
			lastLength = length
			increment = 1 << (16 - length)
		}
		tree.codes[uint32(length)<<16|code>>(16-length)] = uint16(symbol)
		tree.maxLength = max(tree.maxLength, length)
	}
	return tree, nil
}

func (tree *shannonFanoTree) decode(reader *bitReader) (uint16, error) {
	code := uint32(0)
	for length := uint(1); length <= tree.maxLength; length++ {
		bit, err := reader.readBit()
		if err != nil {
			return 0, err
		}
		code = code<<1 | bit
		if symbol, ok := tree.codes[uint32(length)<<16|code]; ok {
			return symbol, nil
		}
	}
	return 0, fmt.Errorf("%w: invalid Shannon-Fano code", errCorruptLegacyZip)
}

// explode decompresses PKZIP Implode (method 6) data of a known uncompressed size.
func explode(compressed io.Reader, flags uint16, size uint64) ([]byte, error) {
	reader := newBitReader(compressed)
	literalTree := (*shannonFanoTree)(nil)
	minimumMatch := uint32(2)
	if flags&implodeFlag3Trees != 0 {
		tree, err := readShannonFanoTree(reader.reader, 256)
		if err != nil {
			return nil, err
		}
		literalTree = tree
		minimumMatch = 3
	}
	lengthTree, err := readShannonFanoTree(reader.reader, 64)
	if err != nil {
		return nil, err
	}
	distanceTree, err := readShannonFanoTree(reader.reader, 64)
	if err != nil {
		return nil, err
	}
	distanceBits := uint(6)
	if flags&implodeFlag8K != 0 {
		distanceBits = 7
	}

	output := make([]byte, 0, size)
	for uint64(len(output)) < size {
		flag, err := reader.readBit()
		if err != nil {
			return nil, err
		}
		if flag == 1 {
			var literal uint32
			if literalTree != nil {
				symbol, err := literalTree.decode(reader)
				if err != nil {
					return nil, err
				}
				literal = uint32(symbol)
			} else if literal, err = reader.readBits(8); err != nil {
				return nil, err
			}
			output = append(output, byte(literal))
			continue
		}

		low, err := reader.readBits(distanceBits)
		if err != nil {
			return nil, err
		}
		high, err := distanceTree.decode(reader)
		if err != nil {
			return nil, err
		}
		distance := int(uint32(high)<<distanceBits|low) + 1
		lengthSymbol, err := lengthTree.decode(reader)
		if err != nil {
			return nil, err
		}
		length := uint32(lengthSymbol)
		if lengthSymbol == 63 {
			extra, err := reader.readBits(8)
			if err != nil {
				return nil, err
			}
			length += extra
		}
		length += minimumMatch
		output = copyMatch(output, distance, int(length), size)
	}
	return output, nil
}

// copyMatch appends length bytes copied from distance bytes back. References before the start
// of the output read as zeros, matching PKZIP's zero-initialized window.
func copyMatch(output []byte, distance int, length int, limit uint64) []byte {
	for range length {
		if uint64(len(output)) >= limit {
			break
		}
		source := len(output) - distance
		if source < 0 {
			output = append(output, 0)
		} else {
			output = append(output, output[source])
		}
	}
	return output
}

// unshrink decompresses PKZIP Shrink (method 1) data: LZW with 9 to 13 bit codes, an explicit
// code-size increase, and partial table clearing.
func unshrink(compressed io.Reader, size uint64) ([]byte, error) {
	const (
		firstCode = 257
		maxCode   = 1 << 13
		escape    = 256
	)
	type entry struct {
		prefix int
		suffix byte
		used   bool
	}
	table := make([]entry, maxCode)
	for code := 0; code < 256; code++ {
		table[code] = entry{prefix: -1, suffix: byte(code), used: true}
	}
	nextFree := func() int {
		for code := firstCode; code < maxCode; code++ {
			if !table[code].used {
				return code
			}
		}
		return -1
	}
	expand := func(code int, scratch []byte) ([]byte, error) {
		scratch = scratch[:0]
		for steps := 0; code >= 0; steps++ {
			if steps > maxCode || !table[code].used {
				return nil, fmt.Errorf("%w: invalid Shrink code", errCorruptLegacyZip)
			}
			scratch = append(scratch, table[code].suffix)
			code = table[code].prefix
		}
		for left, right := 0, len(scratch)-1; left < right; left, right = left+1, right-1 {
			scratch[left], scratch[right] = scratch[right], scratch[left]
		}
		return scratch, nil
	}

	reader := newBitReader(compressed)
	codeSize := uint(9)
	output := make([]byte, 0, size)
	previous := -1
	var scratch []byte
	for uint64(len(output)) < size {
		code, err := reader.readBits(codeSize)
		if err != nil {
			return nil, err
		}
		if code == escape {
			control, err := reader.readBits(codeSize)
			if err != nil {
				return nil, err
			}
			switch control {
			case 1:
				if codeSize >= 13 {
					return nil, fmt.Errorf("%w: Shrink code size overflow", errCorruptLegacyZip)
				}
				codeSize++
			case 2:
				// Partial clear: free every code that is not the prefix of another code.
				referenced := make([]bool, maxCode)
				for index := firstCode; index < maxCode; index++ {
					if table[index].used && table[index].prefix >= 0 {
						referenced[table[index].prefix] = true
					}
				}
				for index := firstCode; index < maxCode; index++ {
					if table[index].used && !referenced[index] {
						table[index] = entry{}
					}
				}
			default:
				return nil, fmt.Errorf("%w: invalid Shrink control code", errCorruptLegacyZip)
			}
			continue
		}

		var sequence []byte
		switch {
		case int(code) < maxCode && table[code].used:
			sequence, err = expand(int(code), scratch)
			if err != nil {
				return nil, err
			}
		case previous >= 0:
			// The code being defined right now: previous sequence plus its own first byte.
			sequence, err = expand(previous, scratch)
			if err != nil {
				return nil, err
			}
			sequence = append(sequence, sequence[0])
		default:
			return nil, fmt.Errorf("%w: invalid Shrink code", errCorruptLegacyZip)
		}
		scratch = sequence

		if previous >= 0 {
			if free := nextFree(); free >= 0 {
				table[free] = entry{prefix: previous, suffix: sequence[0], used: true}
			}
		}
		previous = int(code)
		remaining := size - uint64(len(output))
		if uint64(len(sequence)) > remaining {
			sequence = sequence[:remaining]
		}
		output = append(output, sequence...)
	}
	return output, nil
}
