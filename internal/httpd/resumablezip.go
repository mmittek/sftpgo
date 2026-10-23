// Resumable zip downloads (fork addition). The zip is never written anywhere: it is assembled
// on the fly from the original files, without compression, in a fixed layout, so its size is
// known before the first byte and any byte range can be served again identically. CRC-32
// checksums are computed while the files stream and saved, with checkpoints every 64 MB, in
// the folder's .cache, so a resumed download finds the checksums of everything it already got.
// Every entry and the end of the archive use zip64, so size never changes the format.

package httpd

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	// bump when the byte layout changes, it is part of the ETag
	resumableZipVersion = "1"
	zipChecksumSuffix   = ".zipcrc.json"
	zipCopyBuffer       = 1 << 20
	zipMax32            = 0xffffffff
	zipMax16            = 0xffff
)

var (
	zipChecksumWrites sync.Mutex
	// running CRC-32 values are saved at every multiple of this offset
	zipCheckpointEvery int64 = 64 << 20
)

type zipEntry struct {
	name     string // path inside the archive, "/" terminated for directories
	vpath    string // virtual path for the connection
	dir      bool
	size     int64
	mtime    time.Time
	resolved string // local path, empty when the file is not on a local filesystem
	offset   int64  // offset of the local header
	header   []byte // local header
	crc      uint32
	crcKnown bool
	sums     *zipChecksums
}

func (e *zipEntry) dataStart() int64 { return e.offset + int64(len(e.header)) }

func (e *zipEntry) descriptorLen() int64 {
	if e.dir {
		return 0
	}
	return 24 // zip64: 64-bit sizes
}

func (e *zipEntry) end() int64 { return e.dataStart() + e.size + e.descriptorLen() }

type zipLayout struct {
	entries       []*zipEntry
	centralOffset int64
	centralSize   int64
	total         int64
	etag          string
	modTime       time.Time
}

// zipChecksums is the saved state of a file's CRC-32: the final value and the running value
// at every 64 MB, valid while size and modification time are unchanged
type zipChecksums struct {
	Size        int64             `json:"size"`
	Mtime       int64             `json:"mtime"`
	CRC         *uint32           `json:"crc,omitempty"`
	Checkpoints map[string]uint32 `json:"checkpoints,omitempty"`
}

func zipChecksumPath(resolved string) string {
	return filepath.Join(filepath.Dir(resolved), sharePreviewCacheDir, filepath.Base(resolved)+zipChecksumSuffix)
}

func loadZipChecksums(e *zipEntry) *zipChecksums {
	sums := &zipChecksums{Size: e.size, Mtime: e.mtime.UnixNano(), Checkpoints: map[string]uint32{}}
	if e.resolved == "" {
		return sums
	}
	data, err := os.ReadFile(zipChecksumPath(e.resolved))
	if err != nil {
		return sums
	}
	var saved zipChecksums
	if json.Unmarshal(data, &saved) != nil || saved.Size != e.size || saved.Mtime != e.mtime.UnixNano() {
		return sums
	}
	if saved.Checkpoints == nil {
		saved.Checkpoints = map[string]uint32{}
	}
	return &saved
}

func saveZipChecksums(e *zipEntry) {
	if e.resolved == "" || e.sums == nil {
		return
	}
	zipChecksumWrites.Lock()
	defer zipChecksumWrites.Unlock()
	p := zipChecksumPath(e.resolved)
	if err := os.MkdirAll(filepath.Dir(p), 0o775); err != nil {
		return
	}
	data, err := json.Marshal(e.sums)
	if err != nil {
		return
	}
	if err := writeFileAtomic(p, data); err != nil {
		logger.Debug(logSender, "", "unable to save the zip checksums of %q: %v", e.resolved, err)
	}
}

// collectZipEntries lists the files to archive in a fixed order: the order of the given list,
// then by name inside every directory. Hidden and denied entries are skipped by the connection
func collectZipEntries(conn *Connection, baseDir string, files []string) ([]*zipEntry, error) {
	var entries []*zipEntry
	var walk func(vpath string, info os.FileInfo, depth int) error
	walk = func(vpath string, info os.FileInfo, depth int) error {
		if depth >= util.MaxRecursion {
			return util.ErrRecursionTooDeep
		}
		var err error
		if info == nil {
			if info, err = conn.Stat(vpath, 1); err != nil {
				return err
			}
		}
		name, err := getZipEntryName(vpath, baseDir)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name != "" {
				entries = append(entries, &zipEntry{name: name + "/", vpath: vpath, dir: true, mtime: info.ModTime()})
			}
			lister, err := conn.ReadDir(vpath)
			if err != nil {
				return err
			}
			var contents []os.FileInfo
			for {
				batch, err := lister.Next(vfs.ListerBatchSize)
				finished := errors.Is(err, io.EOF)
				if err != nil && !finished {
					lister.Close()
					return err
				}
				contents = append(contents, batch...)
				if finished {
					break
				}
			}
			lister.Close()
			slices.SortFunc(contents, func(a, b os.FileInfo) int { return strings.Compare(a.Name(), b.Name()) })
			for _, child := range contents {
				if child.IsDir() && child.Name() == sharePreviewCacheDir {
					// the cache holds the checksums this download saves: listing it would change
					// the archive between the first request and a resume
					continue
				}
				if err := walk(util.CleanPath(path.Join(vpath, child.Name())), child, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		entry := &zipEntry{name: name, vpath: vpath, size: info.Size(), mtime: info.ModTime()}
		if fs, resolved, err := conn.GetFsAndResolvedPath(vpath); err == nil && vfs.IsLocalOsFs(fs) {
			entry.resolved = resolved
		}
		entries = append(entries, entry)
		return nil
	}
	for _, file := range files {
		if err := walk(util.CleanPath(path.Join(baseDir, file)), nil, 0); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func buildZipLayout(conn *Connection, baseDir string, files []string) (*zipLayout, error) {
	entries, err := collectZipEntries(conn, baseDir, files)
	if err != nil {
		return nil, err
	}
	// a folder is dated by the newest file inside it: its own modification time changes when
	// this download creates the .cache folder in it, which would change the archive
	for i, e := range entries {
		if !e.dir {
			continue
		}
		var newest time.Time
		for _, inner := range entries[i+1:] {
			if !strings.HasPrefix(inner.name, e.name) {
				break
			}
			if !inner.dir && inner.mtime.After(newest) {
				newest = inner.mtime
			}
		}
		if !newest.IsZero() {
			e.mtime = newest
		}
	}
	layout := &zipLayout{entries: entries}
	tag := sha256.New()
	tag.Write([]byte(resumableZipVersion))
	var offset int64
	for _, e := range entries {
		e.offset = offset
		e.header = zipLocalHeader(e)
		offset = e.end()
		if !e.dir {
			e.sums = loadZipChecksums(e)
			if e.sums.CRC != nil {
				e.crc, e.crcKnown = *e.sums.CRC, true
			}
		}
		fmt.Fprintf(tag, "%s|%t|%d|%d\n", e.name, e.dir, e.size, e.mtime.UnixNano())
		if e.mtime.After(layout.modTime) {
			layout.modTime = e.mtime
		}
	}
	layout.centralOffset = offset
	for _, e := range entries {
		layout.centralSize += int64(len(zipCentralHeader(e)))
	}
	layout.total = layout.centralOffset + layout.centralSize + int64(len(layout.endRecords()))
	layout.etag = `"` + hex.EncodeToString(tag.Sum(nil)[:16]) + `"`
	return layout, nil
}

func zipDOSTime(t time.Time) (uint16, uint16) {
	t = t.UTC()
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	date := uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9)
	clock := uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	return clock, date
}

// zipTimestampExtra is the "UT" extended timestamp field with the modification time in UTC
func zipTimestampExtra(t time.Time) []byte {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint16(b[0:], 0x5455)
	binary.LittleEndian.PutUint16(b[2:], 5)
	b[4] = 1
	binary.LittleEndian.PutUint32(b[5:], uint32(t.Unix()))
	return b
}

func zipFlags(e *zipEntry) uint16 {
	flags := uint16(0x0800) // UTF-8 names
	if !e.dir {
		flags |= 0x0008 // CRC-32 in a data descriptor after the data
	}
	return flags
}

// zipLocalHeader: the CRC-32 follows the data in the descriptor, the sizes are known upfront
// and written here too (zip64 extra field), so streaming readers can extract stored files
func zipLocalHeader(e *zipEntry) []byte {
	extra := zipTimestampExtra(e.mtime)
	version := uint16(45)
	size32 := uint32(0)
	if !e.dir {
		size32 = zipMax32
		z := make([]byte, 20)
		binary.LittleEndian.PutUint16(z[0:], 0x0001)
		binary.LittleEndian.PutUint16(z[2:], 16)
		binary.LittleEndian.PutUint64(z[4:], uint64(e.size))
		binary.LittleEndian.PutUint64(z[12:], uint64(e.size))
		extra = append(extra, z...)
	}
	clock, date := zipDOSTime(e.mtime)
	b := make([]byte, 30, 30+len(e.name)+len(extra))
	binary.LittleEndian.PutUint32(b[0:], 0x04034b50)
	binary.LittleEndian.PutUint16(b[4:], version)
	binary.LittleEndian.PutUint16(b[6:], zipFlags(e))
	binary.LittleEndian.PutUint16(b[8:], 0) // stored
	binary.LittleEndian.PutUint16(b[10:], clock)
	binary.LittleEndian.PutUint16(b[12:], date)
	binary.LittleEndian.PutUint32(b[14:], 0)
	binary.LittleEndian.PutUint32(b[18:], size32)
	binary.LittleEndian.PutUint32(b[22:], size32)
	binary.LittleEndian.PutUint16(b[26:], uint16(len(e.name)))
	binary.LittleEndian.PutUint16(b[28:], uint16(len(extra)))
	b = append(b, e.name...)
	return append(b, extra...)
}

func zipDescriptor(e *zipEntry) []byte {
	b := make([]byte, e.descriptorLen())
	binary.LittleEndian.PutUint32(b[0:], 0x08074b50)
	binary.LittleEndian.PutUint32(b[4:], e.crc)
	binary.LittleEndian.PutUint64(b[8:], uint64(e.size))
	binary.LittleEndian.PutUint64(b[16:], uint64(e.size))
	return b
}

func zipCentralHeader(e *zipEntry) []byte {
	extra := zipTimestampExtra(e.mtime)
	// zip64 extra field: the sizes of files and the offset of every entry
	var z64 []byte
	size32 := uint32(0)
	if !e.dir {
		size32 = zipMax32
		z64 = binary.LittleEndian.AppendUint64(z64, uint64(e.size))
		z64 = binary.LittleEndian.AppendUint64(z64, uint64(e.size))
	}
	offset32 := uint32(zipMax32)
	z64 = binary.LittleEndian.AppendUint64(z64, uint64(e.offset))
	version := uint16(45)
	head := make([]byte, 4)
	binary.LittleEndian.PutUint16(head[0:], 0x0001)
	binary.LittleEndian.PutUint16(head[2:], uint16(len(z64)))
	extra = append(extra, append(head, z64...)...)
	mode := uint32(0o100644)
	attrs := uint32(0)
	if e.dir {
		mode = 0o40755
		attrs = 0x10 // MS-DOS directory
	}
	clock, date := zipDOSTime(e.mtime)
	b := make([]byte, 46, 46+len(e.name)+len(extra))
	binary.LittleEndian.PutUint32(b[0:], 0x02014b50)
	binary.LittleEndian.PutUint16(b[4:], 3<<8|version) // made on unix
	binary.LittleEndian.PutUint16(b[6:], version)
	binary.LittleEndian.PutUint16(b[8:], zipFlags(e))
	binary.LittleEndian.PutUint16(b[10:], 0)
	binary.LittleEndian.PutUint16(b[12:], clock)
	binary.LittleEndian.PutUint16(b[14:], date)
	binary.LittleEndian.PutUint32(b[16:], e.crc)
	binary.LittleEndian.PutUint32(b[20:], size32)
	binary.LittleEndian.PutUint32(b[24:], size32)
	binary.LittleEndian.PutUint16(b[28:], uint16(len(e.name)))
	binary.LittleEndian.PutUint16(b[30:], uint16(len(extra)))
	binary.LittleEndian.PutUint32(b[38:], mode<<16|attrs)
	binary.LittleEndian.PutUint32(b[42:], offset32)
	b = append(b, e.name...)
	return append(b, extra...)
}

func (l *zipLayout) endRecords() []byte {
	var b []byte
	count := uint64(len(l.entries))
	{
		rec := make([]byte, 56)
		binary.LittleEndian.PutUint32(rec[0:], 0x06064b50)
		binary.LittleEndian.PutUint64(rec[4:], 44)
		binary.LittleEndian.PutUint16(rec[12:], 3<<8|45)
		binary.LittleEndian.PutUint16(rec[14:], 45)
		binary.LittleEndian.PutUint64(rec[24:], count)
		binary.LittleEndian.PutUint64(rec[32:], count)
		binary.LittleEndian.PutUint64(rec[40:], uint64(l.centralSize))
		binary.LittleEndian.PutUint64(rec[48:], uint64(l.centralOffset))
		loc := make([]byte, 20)
		binary.LittleEndian.PutUint32(loc[0:], 0x07064b50)
		binary.LittleEndian.PutUint64(loc[8:], uint64(l.centralOffset+l.centralSize))
		binary.LittleEndian.PutUint32(loc[16:], 1)
		b = append(append(b, rec...), loc...)
	}
	end := make([]byte, 22)
	binary.LittleEndian.PutUint32(end[0:], 0x06054b50)
	// the real values are in the zip64 end record
	records, size, offset := uint16(zipMax16), uint32(zipMax32), uint32(zipMax32)
	binary.LittleEndian.PutUint16(end[8:], records)
	binary.LittleEndian.PutUint16(end[10:], records)
	binary.LittleEndian.PutUint32(end[12:], size)
	binary.LittleEndian.PutUint32(end[16:], offset)
	return append(b, end...)
}

// crcAt returns the CRC-32 of the first n bytes of the file, from the nearest saved
// checkpoint, reading only the gap
func (l *zipLayout) crcAt(conn *Connection, e *zipEntry, n int64) (uint32, error) {
	var start int64
	var crc uint32
	for k, v := range e.sums.Checkpoints {
		if o, err := strconv.ParseInt(k, 10, 64); err == nil && o <= n && o > start {
			start, crc = o, v
		}
	}
	if start == n {
		return crc, nil
	}
	reader, err := conn.getFileReader(e.vpath, start, http.MethodGet)
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	w := &zipCRCWriter{l: l, e: e, pos: start, crc: crc}
	if _, err := io.CopyN(w, reader, n-start); err != nil {
		return 0, err
	}
	saveZipChecksums(e)
	return w.crc, nil
}

// ensureCRC makes the final CRC-32 of a file available, reading it if it was never streamed
func (l *zipLayout) ensureCRC(conn *Connection, e *zipEntry) error {
	if e.dir || e.crcKnown {
		return nil
	}
	crc, err := l.crcAt(conn, e, e.size)
	if err != nil {
		return err
	}
	l.setCRC(e, crc)
	return nil
}

func (l *zipLayout) setCRC(e *zipEntry, crc uint32) {
	e.crc, e.crcKnown = crc, true
	e.sums.CRC = &crc
	saveZipChecksums(e)
}

// zipCRCWriter keeps the running CRC-32 of the data it sees and records a checkpoint every
// 64 MB of the file
type zipCRCWriter struct {
	l   *zipLayout
	e   *zipEntry
	pos int64
	crc uint32
}

func (w *zipCRCWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		next := (w.pos/zipCheckpointEvery + 1) * zipCheckpointEvery
		chunk := p
		if int64(len(chunk)) > next-w.pos {
			chunk = p[:next-w.pos]
		}
		w.crc = crc32.Update(w.crc, crc32.IEEETable, chunk)
		w.pos += int64(len(chunk))
		written += len(chunk)
		p = p[len(chunk):]
		if w.pos == next {
			w.e.sums.Checkpoints[strconv.FormatInt(w.pos, 10)] = w.crc
			saveZipChecksums(w.e)
		}
	}
	return written, nil
}

// streamData writes bytes [from, to) of a file, tracking its CRC-32 when the running value at
// from is known or needed: the descriptor of this file is in the response, or the stream
// starts at the beginning of the file
func (l *zipLayout) streamData(w io.Writer, conn *Connection, e *zipEntry, from, to int64, needCRC bool) error {
	track := from == 0 || needCRC
	var crc uint32
	if from > 0 && track {
		var err error
		if crc, err = l.crcAt(conn, e, from); err != nil {
			return err
		}
	}
	reader, err := conn.getFileReader(e.vpath, from, http.MethodGet)
	if err != nil {
		return err
	}
	defer reader.Close()
	dst := w
	tracker := &zipCRCWriter{l: l, e: e, pos: from, crc: crc}
	if track {
		dst = io.MultiWriter(w, tracker)
	}
	buf := make([]byte, zipCopyBuffer)
	if _, err := io.CopyBuffer(dst, io.LimitReader(reader, to-from), buf); err != nil {
		return err
	}
	if track && to == e.size {
		l.setCRC(e, tracker.crc)
	}
	return nil
}

// writeRange writes bytes [start, end] of the archive
func (l *zipLayout) writeRange(w io.Writer, conn *Connection, start, end int64) error {
	part := func(segStart int64, data []byte) error {
		segEnd := segStart + int64(len(data))
		if segEnd <= start || segStart > end {
			return nil
		}
		from, to := max(start, segStart)-segStart, min(end+1, segEnd)-segStart
		_, err := w.Write(data[from:to])
		return err
	}
	for _, e := range l.entries {
		if e.end() <= start {
			continue
		}
		if e.offset > end {
			break
		}
		if err := part(e.offset, e.header); err != nil {
			return err
		}
		if e.dir {
			continue
		}
		dataStart := e.dataStart()
		if dataStart+e.size > start && dataStart <= end && e.size > 0 {
			from := max(start, dataStart) - dataStart
			to := min(end+1, dataStart+e.size) - dataStart
			// the descriptor of this file is in the response: its CRC-32 is needed
			needCRC := end >= dataStart+e.size
			if err := l.streamData(w, conn, e, from, to, needCRC); err != nil {
				return err
			}
		}
		descStart := dataStart + e.size
		if descStart+e.descriptorLen() > start && descStart <= end {
			if err := l.ensureCRC(conn, e); err != nil {
				return err
			}
			if err := part(descStart, zipDescriptor(e)); err != nil {
				return err
			}
		}
	}
	if end < l.centralOffset {
		return nil
	}
	offset := l.centralOffset
	for _, e := range l.entries {
		header := zipCentralHeader(e)
		if offset+int64(len(header)) > start && offset <= end {
			if err := l.ensureCRC(conn, e); err != nil {
				return err
			}
			if err := part(offset, zipCentralHeader(e)); err != nil {
				return err
			}
		}
		offset += int64(len(header))
	}
	return part(offset, l.endRecords())
}

// parseZipRange reads a single "bytes=a-b", "bytes=a-" or "bytes=-n" range
func parseZipRange(header string, total int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		return max(total-n, 0), total - 1, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	end := total - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, total-1)
	}
	return start, end, true
}

// isZipResume tells a resumed download (a range not starting at 0) from a new one, so only
// new downloads count as share uses
func isZipResume(r *http.Request) bool {
	spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes=")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(first, 10, 64)
	return err == nil && start > 0
}

// serveResumableZip streams the archive of the given files with range support. The caller
// sets Content-Disposition
func serveResumableZip(w http.ResponseWriter, r *http.Request, conn *Connection, baseDir string, files []string,
	share *dataprovider.Share,
) {
	_ = conn.User.CheckFsRoot(conn.ID)
	layout, err := buildZipLayout(conn, baseDir, files)
	if err != nil {
		// same as upstream: a zip that cannot be built ends the connection
		conn.Log(logger.LevelDebug, "unable to list the files to zip: %v", err)
		if share != nil {
			_ = dataprovider.UpdateShareLastUse(share, -1)
		}
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", layout.etag)
	w.Header().Set("Last-Modified", layout.modTime.UTC().Format(http.TimeFormat))
	start, end := int64(0), layout.total-1
	status := http.StatusOK
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && r.Method == http.MethodGet && zipIfRange(r, layout) {
		s, e, ok := parseZipRange(rangeHeader, layout.total)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", layout.total))
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end, status = s, e, http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, layout.total))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if err := layout.writeRange(w, conn, start, end); err != nil {
		conn.Log(logger.LevelDebug, "zip download interrupted: %v", err)
		if share != nil && start == 0 {
			_ = dataprovider.UpdateShareLastUse(share, -1)
		}
		panic(http.ErrAbortHandler)
	}
}

// zipIfRange: a range applies only to the same archive, identified by its ETag, or by date
func zipIfRange(r *http.Request, layout *zipLayout) bool {
	ifRange := r.Header.Get("If-Range")
	if ifRange == "" {
		return true
	}
	if strings.HasPrefix(ifRange, `"`) || strings.HasPrefix(ifRange, "W/") {
		return ifRange == layout.etag
	}
	t, err := http.ParseTime(ifRange)
	return err == nil && !layout.modTime.Truncate(time.Second).After(t)
}
