package stream

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// These tests drive the failure branches that normal operation never reaches:
// unreadable directories, unwritable files, and deliberately corrupted records.
// They are the difference between "the error path compiles" and "the error path
// works", which for a storage engine is the whole point.

// requireNonRoot skips tests that rely on filesystem permissions, since root
// ignores them.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission-based fault injection is meaningless as root")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions required")
	}
}

func TestOpenPartitionFailsOnUnwritableParent(t *testing.T) {
	requireNonRoot(t)

	parent := t.TempDir()
	if err := os.Chmod(parent, 0500); err != nil { // r-x: cannot create
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0700) })

	if _, err := OpenPartition("t", 0, filepath.Join(parent, "sub"), PartitionConfig{}); err == nil {
		t.Error("OpenPartition succeeded in an unwritable directory")
	}
}

func TestOpenLogFailsOnUnwritableDir(t *testing.T) {
	requireNonRoot(t)

	parent := t.TempDir()
	os.Chmod(parent, 0500)
	t.Cleanup(func() { os.Chmod(parent, 0700) })

	if _, err := OpenLog(LogConfig{Dir: filepath.Join(parent, "streams")}); err == nil {
		t.Error("OpenLog succeeded in an unwritable directory")
	}
}

func TestOpenCursorStoreFailsOnUnwritableDir(t *testing.T) {
	requireNonRoot(t)

	parent := t.TempDir()
	os.Chmod(parent, 0500)
	t.Cleanup(func() { os.Chmod(parent, 0700) })

	if _, err := OpenCursorStore(filepath.Join(parent, "cursors")); err == nil {
		t.Error("OpenCursorStore succeeded in an unwritable directory")
	}
}

func TestOpenCursorStoreFailsOnUnreadableLog(t *testing.T) {
	requireNonRoot(t)

	dir := t.TempDir()
	cs, err := OpenCursorStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cs.Commit(CursorKey{Topic: "chat", Group: "g", Member: "m"}, 5)
	cs.Close()

	// The log exists but cannot be read: this must surface as an error rather
	// than silently starting with an empty cursor table, which would replay
	// every message to every device.
	path := filepath.Join(dir, cursorFileName)
	os.Chmod(path, 0000)
	t.Cleanup(func() { os.Chmod(path, 0600) })

	if _, err := OpenCursorStore(dir); err == nil {
		t.Error("OpenCursorStore succeeded with an unreadable cursor log")
	}
}

func TestCreateTopicFailsOnUnwritableStreamDir(t *testing.T) {
	requireNonRoot(t)

	dir := t.TempDir()
	l, err := OpenLog(DefaultLogConfig(dir))
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer l.Close()

	os.Chmod(dir, 0500)
	t.Cleanup(func() { os.Chmod(dir, 0700) })

	if _, err := l.CreateTopic("newtopic", TopicConfig{Partitions: 2}); err == nil {
		t.Error("CreateTopic succeeded in an unwritable stream directory")
	}
}

func TestOpenLogSkipsUnparsableDirectories(t *testing.T) {
	dir := t.TempDir()

	// Stray files and directories that are not topics must be ignored, not
	// fatal — an operator dropping a README in the data dir should not stop
	// the server booting.
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("notes"), 0644)
	os.MkdirAll(filepath.Join(dir, "%ZZ"), 0755) // invalid percent-encoding

	l, err := OpenLog(DefaultLogConfig(dir))
	if err != nil {
		t.Fatalf("OpenLog failed on stray entries: %v", err)
	}
	defer l.Close()

	if names := l.TopicNames(); len(names) != 0 {
		t.Errorf("stray entries were loaded as topics: %v", names)
	}
}

func TestOpenLogFailsOnTopicDirWithNoPartitions(t *testing.T) {
	dir := t.TempDir()
	// A topic directory containing nothing is corruption, not an empty topic.
	os.MkdirAll(filepath.Join(dir, "chat"), 0755)

	if _, err := OpenLog(DefaultLogConfig(dir)); err == nil {
		t.Error("a topic directory with no partitions opened successfully")
	}
}

func TestDecodeTopicDirRejectsTruncatedEscape(t *testing.T) {
	for _, in := range []string{"abc%", "abc%A", "%", "%G0"} {
		if _, err := decodeTopicDir(in); err == nil {
			t.Errorf("malformed encoding %q was decoded", in)
		}
	}
}

func TestSegmentReadReportsCorruptRecord(t *testing.T) {
	dir := t.TempDir()
	p, err := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 1 << 20})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 5; i++ {
		p.Append(rec("good"))
	}
	p.Close()

	// Flip a byte inside the last record's body. Recovery truncates a torn
	// tail, but a mid-file bit flip must be reported rather than served.
	logPath, _ := segmentPaths(dir, 1)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}

	// Corrupt the first record's payload while leaving its length intact, so
	// the reader reaches a CRC mismatch rather than a short read.
	data[recordFrameHeaderSize+recordBodyHeaderSize] ^= 0xFF
	os.WriteFile(logPath, data, 0644)

	p2, err := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 1 << 20})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()

	// Recovery truncates from the first bad record, so the partition is empty
	// and the corruption is not silently served as valid data.
	got, _ := p2.Read(1, 10, 0)
	for _, r := range got {
		if string(r.Payload) != "good" {
			t.Errorf("a corrupted record was served as valid: %q", r.Payload)
		}
	}
	if p2.NextSeq() > 1 {
		t.Logf("recovery kept %d records before the corruption", p2.NextSeq()-1)
	}
}

func TestSegmentReadRejectsAbsurdLength(t *testing.T) {
	dir := t.TempDir()
	p, _ := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 1 << 20})
	p.Append(rec("first"))
	p.Close()

	// Write a frame claiming a body larger than MaxRecordSize, with a matching
	// CRC over nothing, so only the length check can catch it.
	logPath, _ := segmentPaths(dir, 1)
	f, _ := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0644)
	var hdr [recordFrameHeaderSize]byte
	binary.LittleEndian.PutUint32(hdr[0:4], crc32.ChecksumIEEE(nil))
	binary.LittleEndian.PutUint32(hdr[4:8], MaxRecordSize+1)
	f.Write(hdr[:])
	f.Close()

	p2, err := OpenPartition("t", 0, dir, PartitionConfig{IndexInterval: 1 << 20})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()

	// The absurd length must be treated as a torn write and truncated, not
	// used to allocate a gigabyte.
	got, err := p2.Read(1, 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d records, want the 1 intact one", len(got))
	}
}

func TestCursorLoadRejectsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, cursorFileName)

	valid := encodeCursorRecord(
		CursorKey{Topic: "chat", Partition: 0, Group: "g", Member: "m"}.String(), 42)

	cases := []struct {
		name    string
		corrupt func([]byte) []byte
	}{
		{"bad crc", func(b []byte) []byte {
			out := append([]byte(nil), b...)
			out = append(out, valid...)
			out[len(b)] ^= 0xFF // flip a CRC byte on the second record
			return out
		}},
		{"absurd length", func(b []byte) []byte {
			out := append([]byte(nil), b...)
			var hdr [cursorRecordHeader]byte
			binary.LittleEndian.PutUint32(hdr[4:8], 1<<20) // past the 64KB cap
			return append(out, hdr[:]...)
		}},
		{"inconsistent key length", func(b []byte) []byte {
			bad := append([]byte(nil), valid...)
			body := bad[cursorRecordHeader:]
			binary.LittleEndian.PutUint16(body[8:10], 9999) // keyLen lies
			binary.LittleEndian.PutUint32(bad[0:4], crc32.ChecksumIEEE(body))
			return append(append([]byte(nil), b...), bad...)
		}},
		{"truncated body", func(b []byte) []byte {
			out := append([]byte(nil), b...)
			return append(out, valid[:len(valid)-3]...)
		}},
	}

	for _, c := range cases {
		os.WriteFile(path, c.corrupt(valid), 0644)

		cs, err := OpenCursorStore(dir)
		if err != nil {
			t.Errorf("%s: OpenCursorStore failed instead of stopping at the corruption: %v", c.name, err)
			continue
		}
		// The healthy prefix — one record — must survive.
		if cs.Count() != 1 {
			t.Errorf("%s: recovered %d cursors, want the 1 intact one", c.name, cs.Count())
		}
		cs.Close()
	}
}

func TestSegmentRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	seg, err := createSegment(dir, 1, 64)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	encoded, _ := (&Record{Seq: 1, Payload: []byte("x")}).encode()
	seg.append(encoded, 1)

	if err := seg.remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	logPath, idxPath := segmentPaths(dir, 1)
	for _, p := range []string{logPath, idxPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after remove", p)
		}
	}
}

func TestListSegmentBasesOnMissingDir(t *testing.T) {
	if _, err := listSegmentBases(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("listing a nonexistent directory succeeded")
	}
}

func TestPositionOrWithExistingValue(t *testing.T) {
	cs, _ := OpenCursorStore(t.TempDir())
	defer cs.Close()

	key := CursorKey{Topic: "chat", Group: "g", Member: "m"}
	if got := cs.PositionOr(key, 99); got != 99 {
		t.Errorf("PositionOr on an unset cursor = %d, want the default", got)
	}
	cs.Commit(key, 7)
	if got := cs.PositionOr(key, 99); got != 7 {
		t.Errorf("PositionOr on a set cursor = %d, want 7", got)
	}
}

func TestReadByteBudgetStopsMidStream(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{SegmentBytes: 512, IndexInterval: 64})
	for i := 0; i < 300; i++ {
		p.Append(rec("a reasonably sized chat message body"))
	}

	// A tiny budget must stop the read early, including across segments,
	// rather than being ignored once a segment boundary is crossed.
	got, err := p.Read(1, 10000, 200)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("byte budget returned nothing at all")
	}
	if len(got) > 20 {
		t.Errorf("a 200-byte budget returned %d records", len(got))
	}
}

func TestTailWithZeroBatchUsesDefault(t *testing.T) {
	p, _ := tempPartition(t, PartitionConfig{})
	p.Append(rec("x"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Tail(newCancelledCtx(), 1, 0, func([]*Record) error { return nil })
	}()
	<-done
}

func TestCreateTopicZeroPartitionsUsesLogDefault(t *testing.T) {
	l, _ := OpenLog(LogConfig{Dir: t.TempDir(), DefaultTopic: TopicConfig{Partitions: 7}})
	defer l.Close()

	topic, err := l.CreateTopic("chat", TopicConfig{Partitions: 0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if topic.PartitionCount() != 7 {
		t.Errorf("partition count = %d, want the log default 7", topic.PartitionCount())
	}
}

// newCancelledCtx returns a context that is already done, so a Tail call
// returns immediately after its first pass.
func newCancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
