package qcow2reader

import (
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

// WarnFunc is called on a warning.
type WarnFunc func(string)

var warnFunc WarnFunc = func(s string) {
	log.Println("go-qcow2reader: WARNING: " + s)
}

// SetWarnFunc sets [WarnFunc].
func SetWarnFunc(fn WarnFunc) {
	warnFunc = fn
}

// Warn prints a warning.
func Warn(a ...any) {
	if warnFunc != nil {
		warnFunc(fmt.Sprint(a...))
	}
}

// Warnf prints a warning.
func Warnf(format string, a ...any) {
	Warn(fmt.Sprintf(format, a...))
}

// DebugPrintFunc is called for debug prints (very verbose).
type DebugPrintFunc func(string)

var debugPrintFunc DebugPrintFunc

// SetDebugPrintFunc sets [DebugPrintFunc].
func SetDebugPrintFunc(fn DebugPrintFunc) {
	debugPrintFunc = fn
}

// DebugPrint prints a debug message.
func DebugPrint(a ...any) {
	if debugPrintFunc != nil {
		debugPrintFunc(fmt.Sprint(a...))
	}
}

// Debugf prints a debug message.
func Debugf(format string, a ...any) {
	DebugPrint(fmt.Sprintf(format, a...))
}

// Magic is the qcow2 magic string.
const Magic = "QFI\xfb"

// MagicType wraps magic bytes.
type MagicType [4]byte

// String implements [fmt.Stringer].
func (x MagicType) String() string {
	return string(x[:])
}

// MarshalText implements [encoding.TextMarshaler].
func (x MagicType) MarshalText() ([]byte, error) {
	return x[:], nil
}

type CryptMethod uint32

const (
	CryptMethodNone = CryptMethod(0)
	CryptMethodAES  = CryptMethod(1)
	CryptMethodLUKS = CryptMethod(2)
)

func (x CryptMethod) String() string {
	switch x {
	case CryptMethodNone:
		return ""
	case CryptMethodAES:
		return "aes"
	case CryptMethodLUKS:
		return "luks"
	default:
		return fmt.Sprintf("unknown-%d", int(x))
	}
}

func (x CryptMethod) MarshalText() ([]byte, error) {
	return []byte(x.String()), nil
}

type HeaderFieldsV2 struct {
	Magic                 MagicType   `json:"magic"`
	Version               uint32      `json:"version"`             // 2 or 3
	BackingFileOffset     uint64      `json:"backing_file_offset"` // offset of file name (not null terminated)
	BackingFileSize       uint32      `json:"backing_file_size"`   // length of file name (<= 1023)
	ClusterBits           uint32      `json:"cluster_bits"`
	Size                  uint64      `json:"size"` // Virtual disk size in bytes
	CryptMethod           CryptMethod `json:"crypt_method"`
	L1Size                uint32      `json:"l1_size"`         // Number of entries
	L1TableOffset         uint64      `json:"l1_table_offset"` // Offset into the image file
	RefcountTableOffset   uint64      `json:"refcount_table_offset"`
	RefcountTableClusters uint32      `json:"refcount_table_clusters"`
	NbSnapshots           uint32      `json:"nb_snapshots"` // Number of snapshots
	SnapshotsOffset       uint64      `json:"snapshots_offset"`
}

type IncompatibleFeatures uint64

const (
	IncompatibleFeaturesDirtyBit             = 0
	IncompatibleFeaturesCorruptBit           = 1
	IncompatibleFeaturesExternalDataFileBit  = 2
	IncompatibleFeaturesCompressionTypeBit   = 3
	IncompatibleFeaturesExtendedL2EntriesBit = 4
)

var IncompatibleFeaturesNames = []string{
	"dirty bit",           // 0
	"corrupt bit",         // 1
	"external data file",  // 2
	"compression type",    // 3
	"extended L2 entries", // 4
}

func activeFeaturesNames(features uint64, names []string) []string {
	var res []string
	for i := 0; i < 64; i++ {
		if (features>>i)&0b1 == 0b1 {
			name := fmt.Sprintf("unknown-%d", i)
			if i < len(names) {
				name = names[i]
			}
			res = append(res, name)
		}
	}
	return res
}

type Features struct {
	Raw   uint64   `json:"raw"`
	Names []string `json:"names"`
}

func newFeatures(x uint64, names []string) *Features {
	if x == 0 {
		return nil
	}
	return &Features{Raw: x, Names: activeFeaturesNames(x, names)}
}

func (x IncompatibleFeatures) MarshalJSON() ([]byte, error) {
	return json.Marshal(newFeatures(uint64(x), IncompatibleFeaturesNames))
}

type CompatibleFeatures uint64

const (
	CompatibleFeaturesLazyRefcountsBit = 0
)

var CompatibleFeaturesNames = []string{
	"lazy refcounts", // 0
}

func (x CompatibleFeatures) MarshalJSON() ([]byte, error) {
	return json.Marshal(newFeatures(uint64(x), CompatibleFeaturesNames))
}

type AutoclearFeatures uint64

const (
	AutoclearFeaturesBitmapsExtensionBit = 0
	AutoclearFeaturesRawExternalBit      = 1
)

var AutoclearFeaturesNames = []string{
	"bitmaps",           // 0
	"raw external data", // 1
}

func (x AutoclearFeatures) MarshalJSON() ([]byte, error) {
	return json.Marshal(newFeatures(uint64(x), AutoclearFeaturesNames))
}

type HeaderFieldsV3 struct {
	IncompatibleFeatures IncompatibleFeatures `json:"incompatible_features"`
	CompatibleFeatures   CompatibleFeatures   `json:"compatible_features"`
	AutoclearFeatures    AutoclearFeatures    `json:"autoclear_features"`
	RefcountOrder        uint32               `json:"refcount_order"`
	HeaderLength         uint32               `json:"header_length"`
}

type CompressionType uint8

const (
	// CompressionTypeZlib is a misnomer. It is actually deflate without zlib header.
	CompressionTypeZlib = CompressionType(0)
	CompressionTypeZstd = CompressionType(1)
)

func (x CompressionType) String() string {
	switch x {
	case CompressionTypeZlib:
		return "zlib" // misnomer; actually deflate without zlib header
	case CompressionTypeZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown-%d", int(x))
	}
}

func (x CompressionType) MarshalText() ([]byte, error) {
	return []byte(x.String()), nil
}

type Decompressor func(r io.Reader) io.ReadCloser

var decompressors = map[CompressionType]Decompressor{
	CompressionTypeZlib: flate.NewReader, // no zlib header
}

// SetDecompressor sets a custom decompressor.
// By default, [flate.NewReader] is registered for [CompressionTypeZlib].
// No decompressor is registered by default for [CompressionTypeZstd].
func SetDecompressor(t CompressionType, d Decompressor) {
	decompressors[t] = d
}

type HeaderFieldsAdditional struct {
	CompressionType CompressionType `json:"compression_type"`
	// Pad is exposed to avoid `panic: reflect: reflect.Value.SetUint using value obtained using unexported field` during [binary.Read].
	Pad [7]byte `json:"-"`
}

type Header struct {
	HeaderFieldsV2
	*HeaderFieldsV3
	*HeaderFieldsAdditional
}

type HeaderExtensionType uint32

const (
	HeaderExtensionTypeEnd                             = HeaderExtensionType(0x00000000)
	HeaderExtensionTypeBackingFileFormatNameString     = HeaderExtensionType(0xe2792aca)
	HeaderExtensionTypeFeatureNameTable                = HeaderExtensionType(0x6803f857)
	HeaderExtensionTypeBitmapsExtension                = HeaderExtensionType(0x23852875)
	HeaderExtensionTypeFullDiskEncryptionHeaderPointer = HeaderExtensionType(0x0537be77)
	HeaderExtensionTypeExternalDataFileNameString      = HeaderExtensionType(0x44415441)
)

type HeaderExtension struct {
	Type   HeaderExtensionType `json:"type"`
	Length uint32              `json:"length"`
}

var (
	ErrNotQcow2               = errors.New("not qcow2")
	ErrUnsupportedBackingFile = errors.New("unsupported backing file")
	ErrUnsupportedEncryption  = errors.New("unsupported encryption method")
	ErrUnsupportedCompression = errors.New("unsupported compression type")
	ErrUnsupportedFeature     = errors.New("unsupported feature")
)

// Readable returns nil if the image is readable, otherwise returns an error.
func (header *Header) Readable() error {
	if string(header.HeaderFieldsV2.Magic[:]) != Magic {
		return ErrNotQcow2
	}
	if header.Version < 2 {
		return ErrNotQcow2
	}
	if header.ClusterBits < 9 {
		return fmt.Errorf("expected cluster bits >= 9, got %d", header.ClusterBits)
	}
	if header.CryptMethod != CryptMethodNone {
		return fmt.Errorf("%w: %q", ErrUnsupportedEncryption, header.CryptMethod)
	}
	if v3 := header.HeaderFieldsV3; v3 != nil {
		for i := 0; i < 64; i++ {
			if (v3.IncompatibleFeatures>>i)&0b1 == 0b1 {
				switch i {
				case IncompatibleFeaturesDirtyBit, IncompatibleFeaturesCorruptBit:
					Warnf("unexpected incompatible feature bit: %q", IncompatibleFeaturesNames[i])
				case IncompatibleFeaturesExternalDataFileBit,
					IncompatibleFeaturesCompressionTypeBit,
					IncompatibleFeaturesExtendedL2EntriesBit:
					return fmt.Errorf("%w: incompatible feature: %q", ErrUnsupportedFeature, IncompatibleFeaturesNames[i])
				default:
					return fmt.Errorf("%w: incompatible feature bit %d", ErrUnsupportedFeature, i)
				}
			}
		}
	}
	if additional := header.HeaderFieldsAdditional; additional != nil {
		if decompressors[additional.CompressionType] == nil {
			return fmt.Errorf("%w (%q)", ErrUnsupportedCompression, additional.CompressionType)
		}
	}
	return nil
}

func readHeader(r io.Reader) (*Header, error) {
	var header Header
	if err := binary.Read(r, binary.BigEndian, &header.HeaderFieldsV2); err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotQcow2, err)
	}
	if string(header.HeaderFieldsV2.Magic[:]) != Magic {
		return nil, fmt.Errorf("%w (the image lacks magic %q)", ErrNotQcow2, Magic)
	}
	switch header.HeaderFieldsV2.Version {
	case 0, 1:
		return nil, fmt.Errorf("%w (expected version >= 2, got %d)", ErrNotQcow2, header.HeaderFieldsV2)
	case 2:
		return &header, nil
	}

	var v3 HeaderFieldsV3
	if err := binary.Read(r, binary.BigEndian, &v3); err != nil {
		return nil, err
	}
	header.HeaderFieldsV3 = &v3

	var additional HeaderFieldsAdditional
	if header.HeaderFieldsV3.HeaderLength > 104 {
		if err := binary.Read(r, binary.BigEndian, &additional); err != nil {
			return nil, err
		}
	}
	header.HeaderFieldsAdditional = &additional
	return &header, nil
}

type l1TableEntry uint64

// l2Offset returns the offset into the image file at which the L2 table starts.
func (x l1TableEntry) l2Offset() uint64 {
	return uint64(x) & 0x00fffffffffffe00
}

func readL1Table(ra io.ReaderAt, offset uint64, entries uint32) ([]l1TableEntry, error) {
	if offset == 0 {
		return nil, errors.New("invalid L1 table offset: 0")
	}
	if entries == 0 {
		return nil, errors.New("invalid L1 table size: 0")
	}
	r := io.NewSectionReader(ra, int64(offset), int64(entries*8))
	l1Table := make([]l1TableEntry, entries)
	if err := binary.Read(r, binary.BigEndian, &l1Table); err != nil {
		return nil, err
	}
	return l1Table, nil
}

type l2TableEntry uint64

func (x l2TableEntry) clusterDescriptor() uint64 {
	return uint64(x) & 0x3fffffffffffffff
}

func (x l2TableEntry) compressed() bool {
	return (x>>62)&0b1 == 0b1
}

/*
// extendedL2TableEntry is not supported yet
type extendedL2TableEntry struct {
	l2TableEntry
	ext uint64
}
*/

func readL2Table(ra io.ReaderAt, offset uint64, clusterSize int) ([]l2TableEntry, error) {
	if offset == 0 {
		return nil, errors.New("invalid L2 table offset: 0")
	}
	r := io.NewSectionReader(ra, int64(offset), int64(clusterSize))
	entries := clusterSize / 8
	l2Table := make([]l2TableEntry, entries)
	if err := binary.Read(r, binary.BigEndian, &l2Table); err != nil {
		return nil, err
	}
	return l2Table, nil
}

type standardClusterDescriptor uint64

func (desc standardClusterDescriptor) allZero() bool {
	return desc&0b1 == 0b1
}

func (desc standardClusterDescriptor) hostClusterOffset() uint64 {
	return uint64(desc) & 0x00fffffffffffe00
}

type compressedClusterDescriptor uint64

func (desc compressedClusterDescriptor) x(clusterBits int) int {
	return 62 - (clusterBits - 8)
}

func (desc compressedClusterDescriptor) hostClusterOffset(clusterBits int) uint64 {
	x := desc.x(clusterBits)
	mask := uint64((1 << x) - 1)
	return uint64(desc) & mask
}

func (desc compressedClusterDescriptor) additionalSectors(clusterBits int) int {
	x := desc.x(clusterBits)
	return int(uint64(desc) >> x)
}

type ImageType string

const (
	ImageTypeRaw   = ImageType("raw")
	ImageTypeQcow2 = ImageType("qcow2")
)

// Image implements [io.ReaderAt] and [io.Closer].
type Image interface {
	io.ReaderAt
	io.Closer
	Type() ImageType
	Size() int64 // -1 if unknown
	Readable() error
}

// ImageInfo wraps [Image] for [json.Marshal].
type ImageInfo struct {
	Type  ImageType `json:"type"`
	Size  int64     `json:"size"`
	Image `json:"image"`
}

// NewImageInfo returns image info.
func NewImageInfo(img Image) *ImageInfo {
	return &ImageInfo{
		Type:  img.Type(),
		Size:  img.Size(),
		Image: img,
	}
}

// Open opens an image.
func Open(ra io.ReaderAt) (Image, error) {
	q, err := OpenQcow2(ra)
	if errors.Is(err, ErrNotQcow2) {
		return OpenRaw(ra)
	}
	return q, err
}

// RawImage implements [Image].
type RawImage struct {
	io.ReaderAt `json:"-"`
}

func (img *RawImage) Close() error {
	if closer, ok := img.ReaderAt.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (img *RawImage) Type() ImageType {
	return ImageTypeRaw
}

func (img *RawImage) Size() int64 {
	if f, ok := img.ReaderAt.(*os.File); ok {
		if st, err := f.Stat(); err == nil {
			return st.Size()
		}
	}
	return -1
}

func (img *RawImage) Readable() error {
	return nil
}

// OpenRaw opens a raw image.
func OpenRaw(ra io.ReaderAt) (*RawImage, error) {
	return &RawImage{ReaderAt: ra}, nil
}

// Qcow2Image implements [Image].
type Qcow2Image struct {
	ra            io.ReaderAt
	Header        `json:"header"`
	errUnreadable error
	clusterSize   int
	l1Table       []l1TableEntry
	decompressor  Decompressor
	BackingFile   string `json:"backing_file"`
	backingImage  Image
}

// OpenQcow2 opens an qcow2 image.
func OpenQcow2(ra io.ReaderAt) (*Qcow2Image, error) {
	img := &Qcow2Image{
		ra: ra,
	}
	r := io.NewSectionReader(ra, 0, -1)
	header, err := readHeader(r)
	if err != nil {
		return nil, fmt.Errorf("faild to read header: %w", err)
	}
	img.Header = *header
	img.errUnreadable = header.Readable() // cache
	if img.errUnreadable == nil {
		img.clusterSize = 1 << header.ClusterBits
		img.l1Table, err = readL1Table(ra, header.L1TableOffset, header.L1Size)
		if err != nil {
			return img, fmt.Errorf("faild to read L1 table: %w", err)
		}
		var compressionType CompressionType
		if header.HeaderFieldsAdditional != nil {
			compressionType = header.HeaderFieldsAdditional.CompressionType
		}
		img.decompressor = decompressors[compressionType]
		if img.decompressor == nil {
			img.errUnreadable = fmt.Errorf("%w (no decompressor is registered for compression type %v)", ErrUnsupportedCompression, compressionType)
			return img, nil
		}
		if header.BackingFileOffset != 0 {
			if header.BackingFileSize > 1023 {
				img.errUnreadable = fmt.Errorf("expected backing file offset <= 1023, got %d", header.BackingFileSize)
				return img, nil
			}
			backingFileNameB := make([]byte, header.BackingFileSize)
			if _, err = ra.ReadAt(backingFileNameB, int64(header.BackingFileOffset)); err != nil {
				img.errUnreadable = fmt.Errorf("failed to read backing file name: %w", err)
				return img, nil
			}
			img.BackingFile = string(backingFileNameB)
			backingFile, err := os.Open(img.BackingFile)
			if err != nil {
				img.errUnreadable = fmt.Errorf("%w (file %q): %v", ErrUnsupportedBackingFile, img.BackingFile, err)
				return img, nil
			}
			img.backingImage, err = Open(backingFile)
			if err != nil {
				img.errUnreadable = fmt.Errorf("%w (file %q): %v", ErrUnsupportedBackingFile, img.BackingFile, err)
				_ = img.backingImage.Close()
				return img, nil
			}
		}
	}
	return img, nil
}

func (img *Qcow2Image) Close() error {
	var err error
	if img.backingImage != nil {
		err = img.backingImage.Close()
	}
	if closer, ok := img.ra.(io.Closer); ok {
		if err2 := closer.Close(); err2 != nil {
			if err != nil {
				Warn(err)
			}
			err = err2
		}
	}
	return err
}

func (img *Qcow2Image) Type() ImageType {
	return ImageTypeQcow2
}

func (img *Qcow2Image) Size() int64 {
	return int64(img.Header.Size)
}

func (img *Qcow2Image) Readable() error {
	return img.errUnreadable
}

// readAtAligned requires that off and off+len(p)-1 belong to the same cluster.
func (img *Qcow2Image) readAtAligned(p []byte, off int64) (int, error) {
	l2Entries := img.clusterSize / 8
	l1Index := int((off / int64(img.clusterSize)) / int64(l2Entries))
	if l1Index >= len(img.l1Table) {
		return 0, fmt.Errorf("index %d exceeds the L1 table length %d", l1Index, img.l1Table)
	}
	l1Entry := img.l1Table[l1Index]
	l2TableOffset := l1Entry.l2Offset()
	if l2TableOffset == 0 {
		return img.readAtAlignedUnallocated(p, off)
	}
	l2Table, err := readL2Table(img.ra, l2TableOffset, img.clusterSize)
	if err != nil {
		return 0, fmt.Errorf("failed to read L2 table for L1 entry %v (index %d): %w", l1Entry, l1Index, err)
	}
	l2Index := int((off / int64(img.clusterSize)) % int64(l2Entries))
	if l2Index >= len(l2Table) {
		return 0, fmt.Errorf("index %d exceeds the L2 table length %d", l2Index, l2Table)
	}
	l2Entry := l2Table[l2Index]
	desc := l2Entry.clusterDescriptor()
	if desc == 0 {
		return img.readAtAlignedUnallocated(p, off)
	}
	var n int
	if l2Entry.compressed() {
		compressedDesc := compressedClusterDescriptor(desc)
		n, err = img.readAtAlignedCompressed(p, off, compressedDesc)
		if err != nil {
			err = fmt.Errorf("failed to read compressed cluster (len=%d, off=%d, desc=0x%X): %w", len(p), off, desc, err)
		}
	} else {
		standardDesc := standardClusterDescriptor(desc)
		n, err = img.readAtAlignedStandard(p, off, standardDesc)
		if err != nil {
			err = fmt.Errorf("failed to read standard cluster (len=%d, off-%d, desc=0x%X): %w", len(p), off, desc, err)
		}
	}
	return n, err
}

func (img *Qcow2Image) readAtAlignedUnallocated(p []byte, off int64) (int, error) {
	if img.backingImage == nil {
		return img.readZero(p, off)
	}
	n, err := img.backingImage.ReadAt(p, off)
	var consumed int
	if n > 0 {
		consumed += n
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if remaining := len(p) - n; remaining > 0 {
		readZeroN, readZeroErr := img.readZero(p[consumed:consumed+remaining], off+int64(consumed))
		if readZeroN > 0 {
			consumed += readZeroN
		}
		if err == nil && readZeroErr != nil {
			err = readZeroErr
		}
	}
	return consumed, err
}

func (img *Qcow2Image) readAtAlignedStandard(p []byte, off int64, desc standardClusterDescriptor) (int, error) {
	if desc.allZero() {
		return img.readZero(p, off)
	}
	hostClusterOffset := desc.hostClusterOffset()
	rawOffset := int64(desc.hostClusterOffset()) + (off % int64(img.clusterSize))
	if rawOffset == 0 {
		return 0, fmt.Errorf("invalid raw offset 0 for virtual offset %d (host cluster offset=%d)", off, hostClusterOffset)
	}
	n, err := img.ra.ReadAt(p, rawOffset)
	if err != nil {
		err = fmt.Errorf("failed to read %d bytes from the raw offset %d: %w", len(p), rawOffset, err)
	}
	return n, err
}

func (img *Qcow2Image) readAtAlignedCompressed(p []byte, off int64, desc compressedClusterDescriptor) (int, error) {
	hostClusterOffset := desc.hostClusterOffset(int(img.Header.ClusterBits))
	if hostClusterOffset == 0 {
		return 0, fmt.Errorf("invalid host cluster offset 0 for virtual offset %d", off)
	}
	additionalSectors := desc.additionalSectors(int(img.Header.ClusterBits))
	compressedSize := img.clusterSize + 512*additionalSectors
	compressedSR := io.NewSectionReader(img.ra, int64(hostClusterOffset), int64(compressedSize))
	zr := img.decompressor(compressedSR)
	defer zr.Close()
	if discard := off % int64(img.clusterSize); discard != 0 {
		if _, err := io.CopyN(io.Discard, zr, discard); err != nil {
			return 0, err
		}
	}
	return zr.Read(p)
}

func (img *Qcow2Image) readZero(p []byte, off int64) (int, error) {
	return readZero(p, off, img.Header.Size)
}

func readZero(p []byte, off int64, sz uint64) (int, error) {
	var err error
	l := len(p)
	if uint64(off+int64(l)) >= sz {
		l = int(sz - uint64(off))
		if l < 0 {
			l = 0
		}
		err = io.EOF
	}
	for i := 0; i < l; i++ {
		p[i] = 0
	}
	return l, err
}

// ReadAt implements [io.ReaderAt].
func (img *Qcow2Image) ReadAt(p []byte, off int64) (n int, err error) {
	if img.errUnreadable != nil {
		err = img.errUnreadable
		return
	}
	if len(p) == 0 {
		return
	}
	remaining := len(p)
	var eof bool
	if uint64(off+int64(remaining)) >= img.Header.Size {
		remaining = int(img.Header.Size - uint64(off))
		eof = true
	}

	for remaining > 0 {
		currentOff := off + int64(n)
		pIndexBegin := n
		pIndexEnd := n + int(img.clusterSize)

		clusterBegin := (off + int64(pIndexBegin)) / int64(img.clusterSize)
		if clusterEnd := (off + int64(pIndexEnd)) / int64(img.clusterSize); clusterEnd != clusterBegin {
			currentSize := off + int64(img.clusterSize) - int64(n)
			pIndexEnd = pIndexBegin + int(currentSize)
		}

		if pIndexEnd > len(p) {
			pIndexEnd = len(p)
		}
		var currentN int
		currentN, err = img.readAtAligned(p[pIndexBegin:pIndexEnd], currentOff)
		if currentN == 0 && err == nil {
			err = io.EOF
		}
		if currentN > 0 {
			n += currentN
			remaining -= currentN
		}
		if err != nil {
			break
		}
	}

	if err == nil && eof {
		err = io.EOF
	}
	return
}
