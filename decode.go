// © 2014 the flac Authors under the MIT license. See AUTHORS for the list of authors.

// Package flac implements a Free Lossless Audio Codec (FLAC) decoder.
package flac

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"strconv"

	"github.com/eaburns/bit"
)

var magic = [4]byte{'f', 'L', 'a', 'C'}

// Decode reads a FLAC file, decodes it, verifies its MD5 checksum, and returns the data and metadata.
func Decode(r io.Reader) ([]byte, MetaData, error) {
	d, err := NewDecoder(r)
	if err != nil {
		return nil, MetaData{}, err
	}

	// Pre-calculate approximate capacity based on audio specs
	expectedSize := d.TotalSamples * int64(d.NChannels) * int64(d.BitsPerSample/8)
	data := make([]byte, 0, expectedSize)
	for {
		frame, err := d.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, MetaData{}, err
		}
		data = append(data, frame...)
	}

	h := md5.New()
	if _, err := h.Write(data); err != nil {
		return nil, MetaData{}, err
	}
	if !bytes.Equal(h.Sum(nil), d.MD5[:]) {
		return nil, MetaData{}, errors.New("Bad MD5 checksum")
	}
	return data, d.MetaData, nil
}

// A Decoder decodes a FLAC audio file.
// Unlike the Decode function, a decoder can decode the file incrementally,
// one frame at a time.
type Decoder struct {
	r *bufio.Reader
	// N is the next frame number.
	n int

	MetaData
	// Add reusable buffers
	rawBuffer   *bytes.Buffer
	frameBuffer []int32
}

// MetaData contains metadata header information from a FLAC file header.
type MetaData struct {
	*StreamInfo
	*VorbisComment
}

// StreamInfo contains information about the FLAC stream.
type StreamInfo struct {
	MinBlock      int
	MaxBlock      int
	MinFrame      int
	MaxFrame      int
	SampleRate    int
	NChannels     int
	BitsPerSample int
	TotalSamples  int64
	MD5           [md5.Size]byte
}

// VorbisComment (a.k.a. FLAC tags) contains Vorbis-style comments that are
// human-readable textual information.
type VorbisComment struct {
	Vendor   string
	Comments []string
}

// NewDecoder reads the FLAC header information and returns a new Decoder.
// If an error is encountered while reading the header information then nil is
// returned along with the error.
func NewDecoder(r io.Reader) (*Decoder, error) {
	br := bufio.NewReaderSize(r, 32*1024)

	err := checkMagic(br)
	if err != nil {
		return nil, err
	}

	d := &Decoder{r: br}
	if d.MetaData, err = readMetaData(d.r); err != nil {
		return nil, err
	}
	if d.StreamInfo == nil {
		return nil, errors.New("Missing STREAMINFO header")
	}

	if d.BitsPerSample != 8 && d.BitsPerSample != 16 && d.BitsPerSample != 24 {
		return nil, errors.New("Unsupported bits per sample (" + strconv.Itoa(d.BitsPerSample) + "), supported values are: 8, 16, and 24")
	}

	return d, nil
}

func checkMagic(r io.Reader) error {
	var m [4]byte
	if _, err := io.ReadFull(r, m[:]); err != nil {
		return err
	}
	if m != magic {
		return errors.New("Bad fLaC magic header")
	}
	return nil
}

type blockType int

const (
	streamInfoType    blockType = 0
	paddingType       blockType = 1
	applicationType   blockType = 2
	seekTableType     blockType = 3
	vorbisCommentType blockType = 4
	cueSheetType      blockType = 5
	pictureType       blockType = 6

	invalidBlockType = 127
)

var blockTypeNames = map[blockType]string{
	streamInfoType:    "STREAMINFO",
	paddingType:       "PADDING",
	applicationType:   "APPLICATION",
	seekTableType:     "SEEKTABLE",
	vorbisCommentType: "VORBIS_COMMENT",
	cueSheetType:      "CUESHEET",
	pictureType:       "PICTURE",
}

func (t blockType) String() string {
	if n, ok := blockTypeNames[t]; ok {
		return n
	}
	if t == invalidBlockType {
		return "InvalidBlockType"
	}
	return "Unknown(" + strconv.Itoa(int(t)) + ")"
}

func readMetaData(r io.Reader) (MetaData, error) {
	var meta MetaData
	for {
		last, kind, n, err := readMetaDataHeader(r)
		if err != nil {
			return meta, errors.New("Failed to read metadata header: " + err.Error())
		}

		header := &io.LimitedReader{R: r, N: int64(n)}

		switch kind {
		case invalidBlockType:
			return meta, errors.New("Invalid metadata block type (127)")

		case streamInfoType:
			meta.StreamInfo, err = readStreamInfo(header)

		case vorbisCommentType:
			meta.VorbisComment, err = readVorbisComment(header)
		}

		if err != nil {
			return meta, err
		}

		// Junk any unread bytes.
		if _, err = io.Copy(ioutil.Discard, header); err != nil {
			return meta, errors.New("Failed to discard metadata: " + err.Error())
		}

		if last {
			break
		}
	}
	return meta, nil
}

func readMetaDataHeader(r io.Reader) (last bool, kind blockType, n int32, err error) {
	const headerSize = 32 // bits
	br := bit.NewReader(&io.LimitedReader{R: r, N: headerSize})
	fs, err := br.ReadFields(1, 7, 24)
	if err != nil {
		return false, 0, 0, err
	}
	return fs[0] == 1, blockType(fs[1]), int32(fs[2]), nil
}

func readStreamInfo(r io.Reader) (*StreamInfo, error) {
	fs, err := bit.NewReader(r).ReadFields(16, 16, 24, 24, 20, 3, 5, 36)
	if err != nil {
		return nil, err
	}
	info := &StreamInfo{
		MinBlock:      int(fs[0]),
		MaxBlock:      int(fs[1]),
		MinFrame:      int(fs[2]),
		MaxFrame:      int(fs[3]),
		SampleRate:    int(fs[4]),
		NChannels:     int(fs[5]) + 1,
		BitsPerSample: int(fs[6]) + 1,
		TotalSamples:  int64(fs[7]),
	}

	csum, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(csum) != md5.Size {
		return nil, errors.New("Bad MD5 checksum size")
	}
	copy(info.MD5[:], csum)

	if info.SampleRate == 0 {
		return info, errors.New("Bad sample rate")
	}

	return info, nil
}

func readVorbisComment(r io.Reader) (*VorbisComment, error) {
	data, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	cmnt := new(VorbisComment)
	cmnt.Vendor, data, err = vorbisString(data)
	if err != nil {
		return nil, err
	}

	if len(data) < 4 {
		return nil, errors.New("invalid vorbis comments header")
	}
	n := binary.LittleEndian.Uint32(data)
	data = data[4:]

	// Pre-allocate comments slice
	cmnt.Comments = make([]string, 0, n)

	for i := uint32(0); i < n; i++ {
		var s string
		s, data, err = vorbisString(data)
		if err != nil {
			return nil, err
		}
		cmnt.Comments = append(cmnt.Comments, s)
	}
	return cmnt, nil
}

func vorbisString(data []byte) (string, []byte, error) {
	if len(data) < 4 {
		return "", nil, errors.New("invalid vorbis string header")
	}
	n := binary.LittleEndian.Uint32(data)
	data = data[4:]
	if uint64(n) > uint64(len(data)) {
		return "", nil, errors.New("vorbis string length exceeds buffer size")
	}
	return string(data[:n]), data[n:], nil
}

// Next returns the audio data from the next frame.
func (d *Decoder) Next() ([]byte, error) {
	defer func() { d.n++ }()

	// Reuse buffer instead of creating new one each time
	if d.rawBuffer == nil {
		d.rawBuffer = bytes.NewBuffer(make([]byte, 0, 4096))
	}
	d.rawBuffer.Reset()
	frame := io.TeeReader(d.r, d.rawBuffer)
	h, err := readFrameHeader(frame, d.StreamInfo)
	if err == io.EOF {
		return nil, err
	} else if err != nil {
		return nil, errors.New("Failed to read the frame header: " + err.Error())
	}

	br := bit.NewReader(frame)
	data := make([][]int32, h.channelAssignment.nChannels())
	for ch := range data {
		data[ch] = make([]int32, h.blockSize)
		if data[ch], err = readSubFrame(br, h, ch); err != nil {
			return nil, err
		}
	}

	// The bit.Reader buffers up to the next byte, so reading from frame occurs
	// on the next byte boundary.  That takes care of the padding to align to the
	// next byte.
	var crc16 [2]byte
	if _, err := io.ReadFull(frame, crc16[:]); err != nil {
		return nil, err
	}
	if err = verifyCRC16(d.rawBuffer.Bytes()); err != nil {
		return nil, err
	}

	fixChannels(data, h.channelAssignment)
	return interleave(data, d.BitsPerSample)
}

func readSubFrame(br *bit.Reader, h *frameHeader, ch int) ([]int32, error) {
	var data []int32
	bps := h.bitsPerSample(ch)

	kind, order, err := readSubFrameHeader(br)
	if err != nil {
		return nil, err
	}
	switch kind {
	case subFrameConstant:
		v, err := br.Read(bps)
		if err != nil {
			return nil, err
		}
		u := signExtend(v, bps)
		data = make([]int32, h.blockSize)
		for j := range data {
			data[j] = u
		}

	case subFrameVerbatim:
		data = make([]int32, h.blockSize)
		for j := range data {
			v, err := br.Read(bps)
			if err != nil {
				return nil, err
			}
			data[j] = signExtend(v, bps)
		}

	case subFrameFixed:
		data, err = decodeFixedSubFrame(br, bps, h.blockSize, order)
		if err != nil {
			return nil, err
		}

	case subFrameLPC:
		data, err = decodeLPCSubFrame(br, bps, h.blockSize, order)
		if err != nil {
			return nil, err
		}

	default:
		return nil, errors.New("Unsupported frame kind")
	}

	return data, nil
}

func fixChannels(data [][]int32, assign channelAssignment) {
	switch assign {
	case leftSide:
		for i, d0 := range data[0] {
			data[1][i] = d0 - data[1][i]
		}

	case rightSide:
		for i, d1 := range data[1] {
			data[0][i] += d1
		}

	case midSide:
		for i, mid := range data[0] {
			side := data[1][i]
			mid *= 2
			mid |= (side & 1) // if side is odd
			data[0][i] = (mid + side) / 2
			data[1][i] = (mid - side) / 2
		}
	}
}

func interleave(chs [][]int32, bps int) ([]byte, error) {
	// Fast path for common stereo 16-bit case
	if bps == 16 && len(chs) == 2 {
		return interleave16BitStereo(chs[0], chs[1])
	}
	nSamples := len(chs[0])
	nChannels := len(chs)

	bytesPerSample := bps / 8
	data := make([]byte, nSamples*nChannels*bytesPerSample)

	switch bps {
	case 8:
		var i int
		for j := 0; j < nSamples; j++ {
			for _, ch := range chs {
				data[i] = byte(ch[j])
				i++
			}
		}
		return data, nil

	case 16:
		var i int
		for j := 0; j < nSamples; j++ {
			for _, ch := range chs {
				s := ch[j]
				data[i] = byte(s & 0xFF)
				data[i+1] = byte((s >> 8) & 0xFF)
				i += 2
			}
		}
		return data, nil

	case 24:
		var i int
		for j := 0; j < nSamples; j++ {
			for _, ch := range chs {
				s := ch[j]
				data[i] = byte(s & 0xFF)
				data[i+1] = byte((s >> 8) & 0xFF)
				data[i+2] = byte((s >> 16) & 0xFF)
				i += 3
			}
		}
		return data, nil

	}
	return nil, errors.New("Unsupported bits per sample")
}

func interleave16BitStereo(left, right []int32) ([]byte, error) {
	nSamples := len(left)
	data := make([]byte, nSamples*4)
	for i, j := 0, 0; i < nSamples; i++ {
		l, r := left[i], right[i]
		data[j] = byte(l & 0xFF)
		data[j+1] = byte((l >> 8) & 0xFF)
		data[j+2] = byte(r & 0xFF)
		data[j+3] = byte((r >> 8) & 0xFF)
		j += 4
	}
	return data, nil
}

type frameHeader struct {
	variableSize      bool
	blockSize         int // Number of inter-channel samples.
	sampleRate        int // In Hz.
	channelAssignment channelAssignment
	sampleSize        int    // Bits
	number            uint64 // Sample number if variableSize is true, otherwise frame number.
	crc8              uint8
}

type channelAssignment int

var (
	leftSide  channelAssignment = 8
	rightSide channelAssignment = 9
	midSide   channelAssignment = 10
)

func (c channelAssignment) nChannels() int {
	n := 2
	if c < 8 {
		n = int(c) + 1
	}
	return n
}

func (h *frameHeader) bitsPerSample(subframe int) uint {
	b := uint(h.sampleSize)
	switch {
	case h.channelAssignment == leftSide && subframe == 1:
		b++
	case h.channelAssignment == rightSide && subframe == 0:
		b++
	case h.channelAssignment == midSide && subframe == 1:
		b++
	}
	return b
}

var (
	blockSizes = [...]int{
		0:  -1, // Reserved.
		1:  192,
		2:  576,
		3:  1152,
		4:  2304,
		5:  4608,
		6:  -1, // Get 8 bit (blocksize-1) from end of header.
		7:  -1, // Get 16 bit (blocksize-1) from end of header.
		8:  256,
		9:  512,
		10: 1024,
		11: 2048,
		12: 4096,
		13: 8192,
		14: 16384,
		15: 23768,
	}

	sampleRates = [...]int{
		0:  -1, // Get from STREAMINFO metadata block.
		1:  88200,
		2:  176400,
		3:  192000,
		4:  8000,
		5:  16000,
		6:  220500,
		7:  24000,
		8:  32000,
		9:  44100,
		10: 48000,
		11: 96000,
		12: -1, // Get 8 bit sample rate (in kHz) from end of header.
		13: -1, // Get 16 bit sample rate (in Hz) from end of header.
		14: -1, // Get 16 bit sample rate (in tens of Hz) from end of header.
		15: -1, // Invalid, to prevent sync-fooling string of 1s.
	}

	sampleSizes = [...]int{
		0: -1, // Get from STREAMINFO metadata block.
		1: 8,
		2: 12,
		3: -1, // Reserved.
		4: 16,
		5: 20,
		6: 24,
		7: -1, // Reserved.
	}
)

func readFrameHeader(r io.Reader, info *StreamInfo) (*frameHeader, error) {
	raw := bytes.NewBuffer(nil)
	br := bit.NewReader(io.TeeReader(r, raw))

	const syncCode = 0x3FFE

	switch sync, err := br.Read(14); {
	case err == nil && sync != syncCode:
		return nil, errors.New("Failed to find the synchronize code for the next frame")
	case err != nil:
		return nil, err
	}

	fs, err := br.ReadFields(1, 1, 4, 4, 4, 3, 1)
	if err != nil {
		return nil, err
	}
	if fs[0] != 0 || fs[6] != 0 {
		return nil, errors.New("Invalid reserved value in frame header")
	}

	h := new(frameHeader)
	h.variableSize = fs[1] == 1

	blockSize := fs[2]

	sampleRate := fs[3]

	h.channelAssignment = channelAssignment(fs[4])
	if h.channelAssignment > midSide {
		return nil, errors.New("Bad channel assignment")
	}

	switch sampleSize := fs[5]; sampleSize {
	case 0:
		h.sampleSize = info.BitsPerSample
	case 3, 7:
		return nil, errors.New("Bad sample size in frame header")
	default:
		h.sampleSize = sampleSizes[sampleSize]
	}

	if h.number, err = utf8Decode(br); err != nil {
		return nil, err
	}

	switch blockSize {
	case 0:
		return nil, errors.New("Bad block size in frame header")
	case 6:
		sz, err := br.Read(8)
		if err != nil {
			return nil, err
		}
		h.blockSize = int(sz) + 1
	case 7:
		sz, err := br.Read(16)
		if err != nil {
			return nil, err
		}
		h.blockSize = int(sz) + 1
	default:
		h.blockSize = blockSizes[blockSize]
	}

	switch sampleRate {
	case 0:
		h.sampleRate = info.SampleRate
	case 12:
		r, err := br.Read(8)
		if err != nil {
			return nil, err
		}
		h.sampleRate = int(r)
	case 13:
		r, err := br.Read(16)
		if err != nil {
			return nil, err
		}
		h.sampleRate = int(r)
	case 14:
		r, err := br.Read(16)
		if err != nil {
			return nil, err
		}
		h.sampleRate = int(r * 10)
	default:
		h.sampleRate = sampleRates[sampleRate]
	}

	crc8, err := br.Read(8)
	if err != nil {
		return nil, err
	}
	h.crc8 = byte(crc8)

	return h, verifyCRC8(raw.Bytes())
}

type subFrameKind int

const (
	subFrameConstant subFrameKind = 0x0
	subFrameVerbatim subFrameKind = 0x1
	subFrameFixed    subFrameKind = 0x8
	subFrameLPC      subFrameKind = 0x20
)

func (k subFrameKind) String() string {
	switch k {
	case subFrameConstant:
		return "SUBFRAME_CONSTANT"
	case subFrameVerbatim:
		return "SUBFRAME_VERBATIM"
	case subFrameFixed:
		return "SUBFRAME_FIXED"
	case subFrameLPC:
		return "SUBFRAME_LPC"
	default:
		return "Unknown(0x" + strconv.FormatInt(int64(k), 16) + ")"
	}
}

func readSubFrameHeader(br *bit.Reader) (kind subFrameKind, order int, err error) {
	switch pad, err := br.Read(1); {
	case err != nil:
		return 0, 0, err
	case pad != 0:
		// Do nothing, but this is a bad padding value.
	}

	switch k, err := br.Read(6); {
	case err != nil:
		return 0, 0, err

	case k == 0:
		kind = subFrameConstant

	case k == 1:
		kind = subFrameVerbatim

	case (k&0x3E == 0x02) || (k&0x3C == 0x04) || (k&0x30 == 0x10):
		return 0, 0, errors.New("Bad subframe type")

	case k&0x38 == 0x08:
		if order = int(k & 0x07); order > 4 {
			return 0, 0, errors.New("Bad subframe type")
		}
		kind = subFrameFixed

	case k&0x20 == 0x20:
		order = int(k&0x1F) + 1
		kind = subFrameLPC

	default:
		return 0, 0, errors.New("Invalid subframe type")
	}

	n := 0
	switch k, err := br.Read(1); {
	case err != nil:
		return 0, 0, err

	case k == 1:
		n++
		k = uint64(0)
		for k == 0 {
			if k, err = br.Read(1); err != nil {
				return 0, 0, err
			}
			n++
		}
	}

	return kind, order, nil
}

var fixedCoeffs = [...][]int32{
	1: {1},
	2: {2, -1},
	3: {3, -3, 1},
	4: {4, -6, 4, -1},
}

func decodeFixedSubFrame(br *bit.Reader, sampleSize uint, blkSize int, predO int) ([]int32, error) {
	warm, err := readInts(br, predO, sampleSize)
	if err != nil {
		return nil, err
	}

	residual, err := decodeResiduals(br, blkSize, predO)
	if err != nil {
		return nil, err
	}

	if predO == 0 {
		return residual, nil
	}

	return lpcDecode(fixedCoeffs[predO], warm, residual, 0), nil
}

func decodeLPCSubFrame(br *bit.Reader, sampleSize uint, blkSize int, predO int) ([]int32, error) {
	warm, err := readInts(br, predO, sampleSize)
	if err != nil {
		return nil, err
	}

	prec, err := br.Read(4)
	if err != nil {
		return nil, err
	} else if prec == 0xF {
		return nil, errors.New("Bad LPC predictor precision")
	}
	prec++

	s, err := br.Read(5)
	if err != nil {
		return nil, err
	}
	shift := int(signExtend(s, 5))
	if shift < 0 {
		return nil, errors.New("Invalid negative shift")
	}

	coeffs, err := readInts(br, predO, uint(prec))
	if err != nil {
		return nil, err
	}

	residual, err := decodeResiduals(br, blkSize, predO)
	if err != nil {
		return nil, err
	}

	return lpcDecode(coeffs, warm, residual, uint(shift)), nil
}

func readInts(br *bit.Reader, n int, bits uint) ([]int32, error) {
	is := make([]int32, n)
	for i := range is {
		w, err := br.Read(bits)
		if err != nil {
			return nil, err
		}
		is[i] = signExtend(w, bits)
	}
	return is, nil
}

func lpcDecode(coeffs, warm, residual []int32, shift uint) []int32 {
	data := make([]int32, len(warm)+len(residual))
	copy(data, warm)
	for i := len(warm); i < len(data); i++ {
		var sum int32
		for j, c := range coeffs {
			sum += c * data[i-j-1]
			data[i] = residual[i-len(warm)] + (sum >> shift)
		}
	}
	return data
}

func decodeResiduals(br *bit.Reader, blkSize int, predO int) ([]int32, error) {
	var bits uint

	switch method, err := br.Read(2); {
	case err != nil:
		return nil, err
	case method == 0:
		bits = 4
	case method == 1:
		bits = 5
	default:
		return nil, errors.New("Bad residual method")
	}

	partO, err := br.Read(4)
	if err != nil {
		return nil, err
	}

	var residue []int32
	for i := 0; i < 1<<partO; i++ {
		M, err := br.Read(bits)
		if err != nil {
			return nil, err
		} else if (bits == 4 && M == 0xF) || (bits == 5 && M == 0x1F) {
			return nil, errors.New("Unsupported, unencoded residuals")
		}

		n := 0
		switch {
		case partO == 0:
			n = blkSize - predO
		case i > 0:
			n = blkSize / (1 << partO)
		default:
			n = (blkSize / (1 << partO)) - predO
		}

		r, err := riceDecode(br, n, uint(M))
		if err != nil {
			return nil, err
		}
		residue = append(residue, r...)
	}
	return residue, nil
}

func signExtend(v uint64, bits uint) int32 {
	if v&(1<<(bits-1)) != 0 {
		return int32(v | (^uint64(0))<<bits)
	}
	return int32(v)
}

func riceDecode(br *bit.Reader, n int, M uint) ([]int32, error) {
	ns := make([]int32, n)
	for i := 0; i < n; i++ {
		var q uint64
		for {
			switch b, err := br.Read(1); {
			case err != nil:
				return nil, err
			case b == 0:
				q++
				continue
			}
			break
		}

		u, err := br.Read(M)
		if err != nil {
			return nil, err
		}

		u |= (q << M)
		ns[i] = int32(u>>1) ^ -int32(u&1)
	}
	return ns, nil
}
