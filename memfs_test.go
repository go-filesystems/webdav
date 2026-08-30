package webdav_test

import (
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"

	filesystem "github.com/go-filesystems/interface"
)

// memFS is an in-memory filesystem.Filesystem used to drive the handler
// without pulling a concrete driver into this module's dependency graph — the
// same separation go-filesystems/nfs keeps between its core and its fat32
// demo, and the reason this module's go.mod names nothing but the interface.
//
// It is adapted from the memFS in go-filesystems/nfs: same shape, same
// failure-injection key ("Op:path"), so a defect found against one server can
// be reproduced against the other with the same fixture.
type memFS struct {
	mu    sync.Mutex
	nodes map[string]*memNode
	// fail injects an error for the named operation on the named path, which
	// is how the error branches of every method are reached.
	fail   map[string]error
	closed bool
}

type memNode struct {
	mode  uint16
	data  []byte
	link  string
	inode uint64
	mtime int64
}

func newMemFS() *memFS {
	return &memFS{
		nodes: map[string]*memNode{"/": {mode: 0o040755, inode: 1}},
		fail:  map[string]error{},
	}
}

// add inserts a node, creating nothing implicitly: a test that forgets the
// parent directory should see the same conflict a real driver would give.
func (m *memFS) add(path string, mode uint16, data []byte) *memFS {
	m.nodes[path] = &memNode{mode: mode, data: data, inode: uint64(len(m.nodes) + 1)}
	return m
}

func (m *memFS) file(path string, data string) *memFS { return m.add(path, 0o100644, []byte(data)) }
func (m *memFS) dir(path string) *memFS               { return m.add(path, 0o040755, nil) }

func (m *memFS) symlink(path, target string) *memFS {
	m.add(path, 0o120777, nil)
	m.nodes[path].link = target
	return m
}

func (m *memFS) failWith(key string, err error) *memFS { m.fail[key] = err; return m }

func (m *memFS) check(op, path string) error { return m.fail[op+":"+path] }

func (m *memFS) Close() error {
	m.closed = true
	return m.fail["Close:"]
}

func (m *memFS) node(path string) (*memNode, error) {
	n, ok := m.nodes[path]
	if !ok {
		return nil, errors.New("memfs: " + path + " not found")
	}
	return n, nil
}

func (m *memFS) Stat(path string) (filesystem.Stat, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("Stat", path); err != nil {
		return nil, err
	}
	n, err := m.node(path)
	if err != nil {
		return nil, err
	}
	if m.fail["NilStat:"+path] != nil {
		// The driver bug the handler must survive: (nil, nil).
		return nil, nil
	}
	return memStat{mode: n.mode, size: uint64(len(n.data)), inode: n.inode, mtime: n.mtime}, nil
}

type memStat struct {
	mode  uint16
	size  uint64
	inode uint64
	mtime int64
}

func (s memStat) Mode() uint16  { return s.mode }
func (s memStat) Size() uint64  { return s.size }
func (s memStat) Inode() uint64 { return s.inode }

// timeStat additionally reports a modification time, which is the optional
// capability webdav.TimeStat probes for.
type timeStat struct{ memStat }

func (s timeStat) ModTime() int64 { return s.mtime }

func (m *memFS) ReadFile(path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("ReadFile", path); err != nil {
		return nil, err
	}
	n, err := m.node(path)
	if err != nil {
		return nil, err
	}
	if n.mode&0o170000 == 0o040000 {
		return nil, errors.New("memfs: is a directory")
	}
	out := make([]byte, len(n.data))
	copy(out, n.data)
	return out, nil
}

func (m *memFS) ListDir(path string) ([]filesystem.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("ListDir", path); err != nil {
		return nil, err
	}
	n, err := m.node(path)
	if err != nil {
		return nil, err
	}
	if n.mode&0o170000 != 0o040000 {
		return nil, errors.New("memfs: not a directory")
	}
	prefix := path
	if prefix != "/" {
		prefix += "/"
	}
	var names []string
	for p := range m.nodes {
		if p == path || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if strings.Contains(rest, "/") {
			continue
		}
		names = append(names, rest)
	}
	sort.Strings(names)
	// A real FAT or ISO directory carries "." and ".." on disk; emitting
	// them here keeps the handler's filtering honest.
	out := []filesystem.DirEntry{
		filesystem.NewDirEntry(0, ".", 0),
		filesystem.NewDirEntry(0, "..", 0),
	}
	for _, name := range names {
		out = append(out, filesystem.NewDirEntry(m.nodes[prefix+name].inode, name, 0))
	}
	return out, nil
}

func (m *memFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("WriteFile", path); err != nil {
		return err
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	if n, ok := m.nodes[path]; ok {
		n.data = cp
		return nil
	}
	m.nodes[path] = &memNode{mode: 0o100000 | uint16(perm&0o7777), data: cp, inode: uint64(len(m.nodes) + 1)}
	return nil
}

func (m *memFS) ReadLink(path string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("ReadLink", path); err != nil {
		return "", err
	}
	n, err := m.node(path)
	if err != nil {
		return "", err
	}
	if n.mode&0o170000 != 0o120000 {
		return "", errors.New("memfs: not a symbolic link")
	}
	return n.link, nil
}

func (m *memFS) MkDir(path string, perm os.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("MkDir", path); err != nil {
		return err
	}
	m.nodes[path] = &memNode{mode: 0o040000 | uint16(perm&0o7777), inode: uint64(len(m.nodes) + 1)}
	return nil
}

func (m *memFS) DeleteFile(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("DeleteFile", path); err != nil {
		return err
	}
	delete(m.nodes, path)
	return nil
}

func (m *memFS) DeleteDir(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("DeleteDir", path); err != nil {
		return err
	}
	delete(m.nodes, path)
	return nil
}

func (m *memFS) Rename(oldPath, newPath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check("Rename", oldPath); err != nil {
		return err
	}
	n, err := m.node(oldPath)
	if err != nil {
		return err
	}
	delete(m.nodes, oldPath)
	m.nodes[newPath] = n
	return nil
}

// timeFS reports a real mtime, satisfying webdav.TimeStat.
type timeFS struct{ *memFS }

func (t *timeFS) Stat(path string) (filesystem.Stat, error) {
	st, err := t.memFS.Stat(path)
	if err != nil || st == nil {
		return st, err
	}
	return timeStat{memStat: st.(memStat)}, nil
}

// --- the Opener capability -------------------------------------------------

// openFS is a memFS that implements filesystem.Opener, so a GET on it is
// answered at the offset the client asked for rather than by materialising
// the whole file.
type openFS struct {
	*memFS
	openErr error
	readErr error
	// nilFile makes OpenFile return (nil, nil), the driver bug the handler
	// has to survive rather than panic on.
	nilFile bool
	// reads counts ReadAt calls, which is how a test proves a Range request
	// did not read the whole file.
	reads int
	bytes int
}

func (o *openFS) OpenFile(path string) (filesystem.File, error) {
	if o.openErr != nil {
		return nil, o.openErr
	}
	if o.nilFile {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.node(path)
	if err != nil {
		return nil, err
	}
	if n.mode&0o170000 == 0o040000 {
		return nil, errors.New("memfs: is a directory")
	}
	return &memFile{fs: o, data: n.data, readErr: o.readErr}, nil
}

type memFile struct {
	fs      *openFS
	data    []byte
	readErr error
	closed  bool
}

func (f *memFile) Size() int64 { return int64(len(f.data)) }
func (f *memFile) Close() error {
	f.closed = true
	return nil
}

// ReadAt follows io.ReaderAt to the letter, which is what interface.File
// requires: a short read is always accompanied by an error.
func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	f.fs.reads++
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	f.fs.bytes += n
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// countingFS counts driver calls, which is how a test proves a PUT costs one
// whole-file write and no read.
type countingFS struct {
	*memFS
	writes, reads int
}

func (c *countingFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	c.writes++
	return c.memFS.WriteFile(path, data, perm)
}

func (c *countingFS) ReadFile(path string) ([]byte, error) {
	c.reads++
	return c.memFS.ReadFile(path)
}
