/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package table

import (
	"crypto/aes"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/pkg/errors"

	"github.com/dgraph-io/badger/v2/options"
	"github.com/dgraph-io/badger/v2/pb"
	"github.com/dgraph-io/badger/v2/y"
	"github.com/dgraph-io/ristretto"
	"github.com/dgraph-io/ristretto/z"
)

const fileSuffix = ".sst"
const intSize = int(unsafe.Sizeof(int(0)))

// Options contains configurable options for Table/Builder.
type Options struct {
	// Options for Opening/Building Table.

	// ChkMode is the checksum verification mode for Table.
	ChkMode options.ChecksumVerificationMode

	// LoadingMode is the mode to be used for loading Table.
	LoadingMode options.FileLoadingMode

	// Options for Table builder.

	// BloomFalsePositive is the false positive probabiltiy of bloom filter.
	BloomFalsePositive float64

	// BlockSize is the size of each block inside SSTable in bytes.
	BlockSize int

	// DataKey is the key used to decrypt the encrypted text.
	DataKey *pb.DataKey

	// Compression indicates the compression algorithm used for block compression.
	Compression options.CompressionType

	Cache *ristretto.Cache
}

// TableInterface is useful for testing.
type TableInterface interface {
	Smallest() []byte
	Biggest() []byte
	DoesNotHave(hash uint64) bool
}

// Table represents a loaded table file with the info we have about it
type Table struct {
	sync.Mutex

	fd        *os.File // Own fd.
	tableSize int      // Initialized in OpenTable, using fd.Stat().

	blockIndex []*pb.BlockOffset
	ref        int32 // For file garbage collection. Atomic.

	mmap []byte // Memory mapped.

	// The following are initialized once and const.
	smallest, biggest []byte // Smallest and largest keys (with timestamps).
	id                uint64 // file id, part of filename

	bf       *z.Bloom
	Checksum []byte

	IsInmemory bool // Set to true if the table is on level 0 and opened in memory.
	opt        *Options
}

// CompressionType returns the compression algorithm used for block compression.
func (t *Table) CompressionType() options.CompressionType {
	return t.opt.Compression
}

// IncrRef increments the refcount (having to do with whether the file should be deleted)
func (t *Table) IncrRef() {
	atomic.AddInt32(&t.ref, 1)
}

// DecrRef decrements the refcount and possibly deletes the table
func (t *Table) DecrRef() error {
	newRef := atomic.AddInt32(&t.ref, -1)
	if newRef == 0 {
		// We can safely delete this file, because for all the current files, we always have
		// at least one reference pointing to them.

		// It's necessary to delete windows files.
		if t.opt.LoadingMode == options.MemoryMap {
			if err := y.Munmap(t.mmap); err != nil {
				return err
			}
			t.mmap = nil
		}
		// fd can be nil if the table belongs to L0 and it is opened in memory. See
		// OpenTableInMemory method.
		if t.fd == nil {
			return nil
		}
		if err := t.fd.Truncate(0); err != nil {
			// This is very important to let the FS know that the file is deleted.
			return err
		}
		filename := t.fd.Name()
		if err := t.fd.Close(); err != nil {
			return err
		}
		if err := os.Remove(filename); err != nil {
			return err
		}
	}
	return nil
}

type block struct {
	offset            int
	data              []byte
	checksum          []byte
	entriesIndexStart int // start index of entryOffsets list
	entryOffsets      []uint32
	chkLen            int // checksum length
}

func (b *block) size() int64 {
	return int64(3*intSize /* Size of the offset, entriesIndexStart and chkLen */ +
		cap(b.data) + cap(b.checksum) + cap(b.entryOffsets)*4)
}

func (b block) verifyCheckSum() error {
	cs := &pb.Checksum{}
	if err := proto.Unmarshal(b.checksum, cs); err != nil {
		return y.Wrapf(err, "unable to unmarshal checksum for block")
	}
	return y.VerifyChecksum(b.data, cs)
}

// OpenTable assumes file has only one table and opens it. Takes ownership of fd upon function
// entry. Returns a table with one reference count on it (decrementing which may delete the file!
// -- consider t.Close() instead). The fd has to writeable because we call Truncate on it before
// deleting. Checksum for all blocks of table is verified based on value of chkMode.
func OpenTable(fd *os.File, opts Options) (*Table, error) {
	fileInfo, err := fd.Stat()
	if err != nil {
		// It's OK to ignore fd.Close() errs in this function because we have only read
		// from the file.
		_ = fd.Close()
		return nil, y.Wrap(err)
	}

	filename := fileInfo.Name()
	id, ok := ParseFileID(filename)
	if !ok {
		_ = fd.Close()
		return nil, errors.Errorf("Invalid filename: %s", filename)
	}
	t := &Table{
		fd:         fd,
		ref:        1, // Caller is given one reference.
		id:         id,
		opt:        &opts,
		IsInmemory: false,
	}

	t.tableSize = int(fileInfo.Size())

	switch opts.LoadingMode {
	case options.LoadToRAM:
		if _, err := t.fd.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		t.mmap = make([]byte, t.tableSize)
		n, err := t.fd.Read(t.mmap)
		if err != nil {
			// It's OK to ignore fd.Close() error because we have only read from the file.
			_ = t.fd.Close()
			return nil, y.Wrapf(err, "Failed to load file into RAM")
		}
		if n != t.tableSize {
			return nil, errors.Errorf("Failed to read all bytes from the file."+
				"Bytes in file: %d Bytes actually Read: %d", t.tableSize, n)
		}
	case options.MemoryMap:
		t.mmap, err = y.Mmap(fd, false, fileInfo.Size())
		if err != nil {
			_ = fd.Close()
			return nil, y.Wrapf(err, "Unable to map file: %q", fileInfo.Name())
		}
	case options.FileIO:
		t.mmap = nil
	default:
		panic(fmt.Sprintf("Invalid loading mode: %v", opts.LoadingMode))
	}

	if err := t.initBiggestAndSmallest(); err != nil {
		return nil, errors.Wrapf(err, "failed to initialize table")
	}
	if opts.ChkMode == options.OnTableRead || opts.ChkMode == options.OnTableAndBlockRead {
		if err := t.VerifyChecksum(); err != nil {
			_ = fd.Close()
			return nil, errors.Wrapf(err, "failed to verify checksum")
		}
	}

	return t, nil
}

// OpenInMemoryTable is similar to OpenTable but it opens a new table from the provided data.
// OpenInMemoryTable is used for L0 tables.
func OpenInMemoryTable(data []byte, id uint64, opt *Options) (*Table, error) {
	opt.LoadingMode = options.LoadToRAM
	t := &Table{
		ref:        1, // Caller is given one reference.
		opt:        opt,
		mmap:       data,
		tableSize:  len(data),
		IsInmemory: true,
		id:         id, // It is important that each table gets a unique ID.
	}

	if err := t.initBiggestAndSmallest(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Table) initBiggestAndSmallest() error {
	if err := t.readIndex(); err != nil {
		return errors.Wrapf(err, "failed to read index.")
	}

	t.smallest = t.blockIndex[0].Key

	it2 := t.NewIterator(true)
	defer it2.Close()
	it2.Rewind()
	if !it2.Valid() {
		return errors.Wrapf(it2.err, "failed to initialize biggest for table %s", t.Filename())
	}
	t.biggest = it2.Key()
	return nil
}

// Close closes the open table.  (Releases resources back to the OS.)
func (t *Table) Close() error {
	if t.opt.LoadingMode == options.MemoryMap {
		if err := y.Munmap(t.mmap); err != nil {
			return err
		}
		t.mmap = nil
	}
	if t.fd == nil {
		return nil
	}
	return t.fd.Close()
}

func (t *Table) read(off, sz int) ([]byte, error) {
	if len(t.mmap) > 0 {
		if len(t.mmap[off:]) < sz {
			return nil, y.ErrEOF
		}
		return t.mmap[off : off+sz], nil
	}

	res := make([]byte, sz)
	nbr, err := t.fd.ReadAt(res, int64(off))
	y.NumReads.Add(1)
	y.NumBytesRead.Add(int64(nbr))
	return res, err
}

func (t *Table) readNoFail(off, sz int) []byte {
	res, err := t.read(off, sz)
	y.Check(err)
	return res
}

func (t *Table) readIndex() error {
	readPos := t.tableSize

	// Read checksum len from the last 4 bytes.
	readPos -= 4
	buf := t.readNoFail(readPos, 4)
	checksumLen := int(y.BytesToU32(buf))

	// Read checksum.
	expectedChk := &pb.Checksum{}
	readPos -= checksumLen
	buf = t.readNoFail(readPos, checksumLen)
	if err := proto.Unmarshal(buf, expectedChk); err != nil {
		return err
	}

	// Read index size from the footer.
	readPos -= 4
	buf = t.readNoFail(readPos, 4)
	indexLen := int(y.BytesToU32(buf))
	// Read index.
	readPos -= indexLen
	data := t.readNoFail(readPos, indexLen)

	if err := y.VerifyChecksum(data, expectedChk); err != nil {
		return y.Wrapf(err, "failed to verify checksum for table: %s", t.Filename())
	}

	index := pb.TableIndex{}
	// Decrypt the table index if it is encrypted.
	if t.shouldDecrypt() {
		var err error
		if data, err = t.decrypt(data); err != nil {
			return y.Wrapf(err,
				"Error while decrypting table index for the table %d in Table.readIndex", t.id)
		}
	}
	err := proto.Unmarshal(data, &index)
	y.Check(err)

	t.bf = z.JSONUnmarshal(index.BloomFilter)
	t.blockIndex = index.Offsets
	return nil
}

func (t *Table) block(idx int) (*block, error) {
	y.AssertTruef(idx >= 0, "idx=%d", idx)
	if idx >= len(t.blockIndex) {
		return nil, errors.New("block out of index")
	}
	if t.opt.Cache != nil {
		key := t.blockCacheKey(idx)
		blk, ok := t.opt.Cache.Get(key)
		if ok && blk != nil {
			return blk.(*block), nil
		}
	}
	ko := t.blockIndex[idx]
	blk := &block{
		offset: int(ko.Offset),
	}
	var err error
	if blk.data, err = t.read(blk.offset, int(ko.Len)); err != nil {
		return nil, errors.Wrapf(err,
			"failed to read from file: %s at offset: %d, len: %d", t.fd.Name(), blk.offset, ko.Len)
	}

	if t.shouldDecrypt() {
		// Decrypt the block if it is encrypted.
		if blk.data, err = t.decrypt(blk.data); err != nil {
			return nil, err
		}
	}

	blk.data, err = t.decompressData(blk.data)
	if err != nil {
		return nil, errors.Wrapf(err,
			"failed to decode compressed data in file: %s at offset: %d, len: %d",
			t.fd.Name(), blk.offset, ko.Len)
	}

	// Read meta data related to block.
	readPos := len(blk.data) - 4 // First read checksum length.
	blk.chkLen = int(y.BytesToU32(blk.data[readPos : readPos+4]))

	// Checksum length greater than block size could happen if the table was compressed and
	// it was opened with an incorrect compression algorithm (or the data was corrupted).
	if blk.chkLen > len(blk.data) {
		return nil, errors.New("invalid checksum length. Either the data is" +
			"corrupted or the table options are incorrectly set")
	}

	// Read checksum and store it
	readPos -= blk.chkLen
	blk.checksum = blk.data[readPos : readPos+blk.chkLen]
	// Move back and read numEntries in the block.
	readPos -= 4
	numEntries := int(y.BytesToU32(blk.data[readPos : readPos+4]))
	entriesIndexStart := readPos - (numEntries * 4)
	entriesIndexEnd := entriesIndexStart + numEntries*4

	blk.entryOffsets = y.BytesToU32Slice(blk.data[entriesIndexStart:entriesIndexEnd])

	blk.entriesIndexStart = entriesIndexStart

	// Drop checksum and checksum length.
	// The checksum is calculated for actual data + entry index + index length
	blk.data = blk.data[:readPos+4]

	// Verify checksum on if checksum verification mode is OnRead on OnStartAndRead.
	if t.opt.ChkMode == options.OnBlockRead || t.opt.ChkMode == options.OnTableAndBlockRead {
		if err = blk.verifyCheckSum(); err != nil {
			return nil, err
		}
	}
	if t.opt.Cache != nil {
		key := t.blockCacheKey(idx)
		t.opt.Cache.Set(key, blk, blk.size())
	}
	return blk, nil
}

func (t *Table) blockCacheKey(idx int) uint64 {
	y.AssertTrue(t.ID() < math.MaxUint32)
	y.AssertTrue(uint32(idx) < math.MaxUint32)
	return (t.ID() << 32) | uint64(idx)
}

// Size is its file size in bytes
func (t *Table) Size() int64 { return int64(t.tableSize) }

// Smallest is its smallest key, or nil if there are none
func (t *Table) Smallest() []byte { return t.smallest }

// Biggest is its biggest key, or nil if there are none
func (t *Table) Biggest() []byte { return t.biggest }

// Filename is NOT the file name.  Just kidding, it is.
func (t *Table) Filename() string { return t.fd.Name() }

// ID is the table's ID number (used to make the file name).
func (t *Table) ID() uint64 { return t.id }

// DoesNotHave returns true if (but not "only if") the table does not have the key hash.
// It does a bloom filter lookup.
func (t *Table) DoesNotHave(hash uint64) bool { return !t.bf.Has(hash) }

// VerifyChecksum verifies checksum for all blocks of table. This function is called by
// OpenTable() function. This function is also called inside levelsController.VerifyChecksum().
func (t *Table) VerifyChecksum() error {
	for i, os := range t.blockIndex {
		b, err := t.block(i)
		if err != nil {
			return y.Wrapf(err, "checksum validation failed for table: %s, block: %d, offset:%d",
				t.Filename(), i, os.Offset)
		}

		// OnBlockRead or OnTableAndBlockRead, we don't need to call verify checksum
		// on block, verification would be done while reading block itself.
		if !(t.opt.ChkMode == options.OnBlockRead || t.opt.ChkMode == options.OnTableAndBlockRead) {
			if err = b.verifyCheckSum(); err != nil {
				return y.Wrapf(err,
					"checksum validation failed for table: %s, block: %d, offset:%d",
					t.Filename(), i, os.Offset)
			}
		}
	}

	return nil
}

// shouldDecrypt tells whether to decrypt or not. We decrypt only if the datakey exist
// for the table.
func (t *Table) shouldDecrypt() bool {
	return t.opt.DataKey != nil
}

// KeyID returns data key id.
func (t *Table) KeyID() uint64 {
	if t.opt.DataKey != nil {
		return t.opt.DataKey.KeyId
	}
	// By default it's 0, if it is plain text.
	return 0
}

// decrypt decrypts the given data. It should be called only after checking shouldDecrypt.
func (t *Table) decrypt(data []byte) ([]byte, error) {
	// Last BlockSize bytes of the data is the IV.
	iv := data[len(data)-aes.BlockSize:]
	// Rest all bytes are data.
	data = data[:len(data)-aes.BlockSize]
	return y.XORBlock(data, t.opt.DataKey.Data, iv)
}

// ParseFileID reads the file id out of a filename.
func ParseFileID(name string) (uint64, bool) {
	name = path.Base(name)
	if !strings.HasSuffix(name, fileSuffix) {
		return 0, false
	}
	//	suffix := name[len(fileSuffix):]
	name = strings.TrimSuffix(name, fileSuffix)
	id, err := strconv.Atoi(name)
	if err != nil {
		return 0, false
	}
	y.AssertTrue(id >= 0)
	return uint64(id), true
}

// IDToFilename does the inverse of ParseFileID
func IDToFilename(id uint64) string {
	return fmt.Sprintf("%06d", id) + fileSuffix
}

// NewFilename should be named TableFilepath -- it combines the dir with the ID to make a table
// filepath.
func NewFilename(id uint64, dir string) string {
	return filepath.Join(dir, IDToFilename(id))
}

// decompressData decompresses the given data.
func (t *Table) decompressData(data []byte) ([]byte, error) {
	switch t.opt.Compression {
	case options.None:
		return data, nil
	case options.Snappy:
		return snappy.Decode(nil, data)
	case options.ZSTD:
		return y.ZSTDDecompress(nil, data)
	}
	return nil, errors.New("Unsupported compression type")
}
