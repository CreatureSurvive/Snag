package server

import (
	"bytes"
	"debug/dwarf"
	"debug/macho"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Symbols turn MetricKit frames (a binary UUID plus an offset) into function names and source
// lines, using dSYMs uploaded per project. Files live at <data>/symbols/<project>/<UUID>/.

// SymbolMeta describes one uploaded binary slice.
type SymbolMeta struct {
	UUID       string    `json:"uuid"`
	Binary     string    `json:"binary"`
	Arch       string    `json:"arch"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploadedAt"`
}

// SymbolStore keeps uploaded symbols and a small cache of parsed ones.
type SymbolStore struct {
	dir string

	mu     sync.Mutex
	loaded map[string]*symbolFile // project|UUID
	order  []string               // least recently used first
}

const symbolCacheSize = 8

func OpenSymbols(dataDir string) *SymbolStore {
	return &SymbolStore{dir: filepath.Join(dataDir, "symbols"), loaded: map[string]*symbolFile{}}
}

func normalizeUUID(u string) string { return strings.ToUpper(strings.TrimSpace(u)) }

func (ss *SymbolStore) path(project, uuid string) string {
	return filepath.Join(ss.dir, project, normalizeUUID(uuid))
}

// List returns a project's uploaded symbols, newest first.
func (ss *SymbolStore) List(project string) []SymbolMeta {
	entries, _ := os.ReadDir(filepath.Join(ss.dir, project))
	var out []SymbolMeta
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(ss.dir, project, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m SymbolMeta
		if json.Unmarshal(data, &m) == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UploadedAt.After(out[j].UploadedAt) })
	return out
}

func (ss *SymbolStore) Has(project, uuid string) bool {
	_, err := os.Stat(filepath.Join(ss.path(project, uuid), "macho"))
	return err == nil
}

func (ss *SymbolStore) Delete(project, uuid string) error {
	if !ValidID(uuid) {
		return errors.New("not a UUID")
	}
	ss.mu.Lock()
	delete(ss.loaded, project+"|"+normalizeUUID(uuid))
	ss.mu.Unlock()
	return os.RemoveAll(ss.path(project, uuid))
}

// DeleteProject removes every symbol file of a project.
func (ss *SymbolStore) DeleteProject(project string) error {
	ss.mu.Lock()
	for k := range ss.loaded {
		if strings.HasPrefix(k, project+"|") {
			delete(ss.loaded, k)
		}
	}
	ss.mu.Unlock()
	return os.RemoveAll(filepath.Join(ss.dir, project))
}

var errNoDebugInfo = errors.New("no Mach-O slices with DWARF debug info found; upload the .dSYM (zipped) or the DWARF file inside it")

// Add stores every Mach-O slice with debug info found in data: a Mach-O file (thin or fat) or a
// zip containing .dSYM bundles.
func (ss *SymbolStore) Add(project string, data []byte) ([]SymbolMeta, error) {
	var objects [][]byte
	if bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		var err error
		if objects, err = machOsInZip(data); err != nil {
			return nil, err
		}
	} else {
		objects = [][]byte{data}
	}
	var added []SymbolMeta
	for _, obj := range objects {
		for _, slice := range machOSlices(obj) {
			f, err := macho.NewFile(bytes.NewReader(slice.data))
			if err != nil {
				continue
			}
			uuid := machOUUID(f)
			hasDWARF := f.Section("__debug_info") != nil
			_ = f.Close()
			if uuid == "" || !hasDWARF {
				continue
			}
			meta := SymbolMeta{UUID: uuid, Binary: slice.name, Arch: slice.arch, Size: int64(len(slice.data)), UploadedAt: time.Now().UTC()}
			dir := ss.path(project, uuid)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return added, err
			}
			if err := os.WriteFile(filepath.Join(dir, "macho.tmp"), slice.data, 0o640); err != nil {
				return added, err
			}
			if err := os.Rename(filepath.Join(dir, "macho.tmp"), filepath.Join(dir, "macho")); err != nil {
				return added, err
			}
			if err := writeJSON(filepath.Join(dir, "meta.json"), meta); err != nil {
				return added, err
			}
			ss.mu.Lock()
			delete(ss.loaded, project+"|"+uuid)
			ss.mu.Unlock()
			added = append(added, meta)
		}
	}
	if len(added) == 0 {
		return nil, errNoDebugInfo
	}
	return added, nil
}

type machOSlice struct {
	name, arch string
	data       []byte
}

// machOSlices splits a fat file into its architectures; a thin file is one slice.
func machOSlices(data []byte) []machOSlice {
	if fat, err := macho.NewFatFile(bytes.NewReader(data)); err == nil {
		defer fat.Close()
		var out []machOSlice
		for _, a := range fat.Arches {
			end := uint64(a.Offset) + uint64(a.Size)
			if end > uint64(len(data)) {
				continue
			}
			out = append(out, machOSlice{arch: archName(a.Cpu, a.SubCpu), data: data[a.Offset:end]})
		}
		return out
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	defer f.Close()
	return []machOSlice{{arch: archName(f.Cpu, f.SubCpu), data: data}}
}

func archName(cpu macho.Cpu, sub uint32) string {
	switch cpu {
	case macho.CpuArm64:
		if sub&0xff == 2 {
			return "arm64e"
		}
		return "arm64"
	case macho.CpuAmd64:
		return "x86_64"
	case macho.CpuArm:
		return "armv7"
	}
	return strings.ToLower(strings.TrimPrefix(cpu.String(), "Cpu"))
}

const lcUUID = 0x1b

func machOUUID(f *macho.File) string {
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) >= 24 && f.ByteOrder.Uint32(raw[0:4]) == lcUUID {
			u := raw[8:24]
			return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
		}
	}
	return ""
}

// machOsInZip returns the DWARF files of the .dSYM bundles in a zip, each labelled with its
// binary name (the file name).
func machOsInZip(data []byte) ([][]byte, error) {
	zr, err := zipReader(data)
	if err != nil {
		return nil, fmt.Errorf("reading zip: %w", err)
	}
	var out [][]byte
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.Contains(f.Name, ".dSYM/Contents/Resources/DWARF/") {
			continue
		}
		total += int64(f.UncompressedSize64)
		if total > maxRestoreFileBytes {
			return nil, errors.New("the dSYMs are larger than 1 GB uncompressed")
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxRestoreFileBytes))
		rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// symbolFile is a parsed dSYM slice.
type symbolFile struct {
	data     *dwarf.Data
	textAddr uint64
	// funcs holds every function range (depth 0) and inlined call range (depth 1+), sorted by
	// low address then depth. A function's inlined calls follow it, before the next function.
	funcs []symFunc
	units []unitRange
}

type symFunc struct {
	low, high uint64
	name      string
	depth     int
}

type unitRange struct {
	low, high uint64
	unit      *dwarf.Entry
}

// Frame is one symbolicated frame.
type SymbolicatedFrame struct {
	Function string `json:"function"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	// Inlined lists the functions this one was inlined into, innermost first.
	InlinedInto []string `json:"inlinedInto,omitempty"`
}

func (ss *SymbolStore) load(project, uuid string) (*symbolFile, error) {
	key := project + "|" + normalizeUUID(uuid)
	ss.mu.Lock()
	if f, ok := ss.loaded[key]; ok {
		ss.touch(key)
		ss.mu.Unlock()
		return f, nil
	}
	ss.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(ss.path(project, uuid), "macho"))
	if err != nil {
		return nil, err
	}
	sf, err := parseSymbolFile(data)
	if err != nil {
		return nil, err
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.loaded[key] = sf
	ss.touch(key)
	for len(ss.order) > symbolCacheSize {
		delete(ss.loaded, ss.order[0])
		ss.order = ss.order[1:]
	}
	return sf, nil
}

func (ss *SymbolStore) touch(key string) {
	for i, k := range ss.order {
		if k == key {
			ss.order = append(ss.order[:i], ss.order[i+1:]...)
			break
		}
	}
	ss.order = append(ss.order, key)
}

// Lookup symbolicates offset (MetricKit's offsetIntoBinaryTextSegment) in the binary with uuid.
// returnAddress is true for every frame but the crashing one: those point after the call.
func (ss *SymbolStore) Lookup(project, uuid string, offset uint64, returnAddress bool) (SymbolicatedFrame, bool) {
	sf, err := ss.load(project, uuid)
	if err != nil {
		return SymbolicatedFrame{}, false
	}
	pc := sf.textAddr + offset
	if returnAddress && pc > sf.textAddr {
		pc--
	}
	return sf.lookup(pc)
}

func parseSymbolFile(data []byte) (*symbolFile, error) {
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	text := f.Segment("__TEXT")
	if text == nil {
		return nil, errors.New("no __TEXT segment")
	}
	d, err := f.DWARF()
	if err != nil {
		return nil, err
	}
	sf := &symbolFile{data: d, textAddr: text.Addr}

	// One pass over every entry: qualified names for types and declarations, and the ranges of
	// functions and their inlined calls. Names are resolved afterwards, since a function can
	// refer to a declaration that comes later.
	type scope struct {
		prefix string
		depth  int // -1 outside any function
	}
	type found struct {
		symFunc
		own string
		ref dwarf.Offset
	}
	names := map[dwarf.Offset]string{}
	var all []found
	stack := []scope{{depth: -1}}
	r := d.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			break
		}
		if e.Tag == 0 {
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		parent := stack[len(stack)-1]
		name, _ := e.Val(dwarf.AttrName).(string)
		qualified := name
		if parent.prefix != "" && name != "" {
			qualified = parent.prefix + "." + name
		}
		if name != "" {
			names[e.Offset] = qualified
		}
		self := parent
		switch e.Tag {
		case dwarf.TagCompileUnit:
			self = scope{depth: -1}
			if ranges, err := d.Ranges(e); err == nil {
				for _, rg := range ranges {
					sf.units = append(sf.units, unitRange{rg[0], rg[1], e})
				}
			}
		case dwarf.TagModule:
			// Swift modules and Clang modules: leave the module name out, like atos.
		case dwarf.TagStructType, dwarf.TagClassType, dwarf.TagUnionType, dwarf.TagEnumerationType,
			dwarf.TagNamespace, dwarf.TagInterfaceType:
			if name != "" {
				self.prefix = qualified
			}
		case dwarf.TagSubprogram, dwarf.TagInlinedSubroutine:
			ranges, _ := d.Ranges(e)
			if len(ranges) == 0 || (e.Tag == dwarf.TagInlinedSubroutine && parent.depth < 0) {
				break // a declaration, or an inlined call outside any function
			}
			depth := 0
			if e.Tag == dwarf.TagInlinedSubroutine {
				depth = parent.depth + 1
			}
			var ref dwarf.Offset
			if o, ok := e.Val(dwarf.AttrSpecification).(dwarf.Offset); ok {
				ref = o
			} else if o, ok := e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset); ok {
				ref = o
			}
			for _, rg := range ranges {
				if rg[1] > rg[0] {
					all = append(all, found{symFunc: symFunc{low: rg[0], high: rg[1], depth: depth}, own: qualified, ref: ref})
				}
			}
			self.depth = depth
		}
		if e.Children {
			stack = append(stack, self)
		}
	}
	// Follow specification/abstract-origin chains (an abstract origin can itself point at a
	// specification) to the declaration that carries the qualified name.
	resolve := func(off dwarf.Offset) string {
		for i := 0; i < 4 && off != 0; i++ {
			if n, ok := names[off]; ok && strings.Contains(n, ".") {
				return n
			}
			r.Seek(off)
			e, err := r.Next()
			if err != nil || e == nil {
				break
			}
			next, _ := e.Val(dwarf.AttrSpecification).(dwarf.Offset)
			if next == 0 {
				next, _ = e.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset)
			}
			if next == 0 {
				return names[off]
			}
			off = next
		}
		return names[off]
	}
	sf.funcs = make([]symFunc, 0, len(all))
	for _, fn := range all {
		name := fn.own
		if fn.ref != 0 {
			if n := resolve(fn.ref); n != "" {
				name = n
			}
		}
		if name == "" {
			continue
		}
		fn.name = name
		sf.funcs = append(sf.funcs, fn.symFunc)
	}
	sort.SliceStable(sf.funcs, func(i, j int) bool {
		if sf.funcs[i].low != sf.funcs[j].low {
			return sf.funcs[i].low < sf.funcs[j].low
		}
		return sf.funcs[i].depth < sf.funcs[j].depth
	})
	sort.Slice(sf.units, func(i, j int) bool { return sf.units[i].low < sf.units[j].low })
	return sf, nil
}

func (sf *symbolFile) lookup(pc uint64) (SymbolicatedFrame, bool) {
	// The last range starting at or before pc, then back to its enclosing function.
	i := sort.Search(len(sf.funcs), func(i int) bool { return sf.funcs[i].low > pc }) - 1
	var chain []symFunc // containing ranges, innermost last
	for ; i >= 0; i-- {
		fn := sf.funcs[i]
		if pc >= fn.low && pc < fn.high {
			if len(chain) == 0 || fn.depth < chain[0].depth {
				chain = append([]symFunc{fn}, chain...)
			}
		}
		if fn.depth == 0 && pc >= fn.low {
			break
		}
	}
	if len(chain) == 0 || chain[0].depth != 0 {
		return SymbolicatedFrame{}, false
	}
	out := SymbolicatedFrame{Function: chain[len(chain)-1].name}
	for j := len(chain) - 2; j >= 0; j-- {
		out.InlinedInto = append(out.InlinedInto, chain[j].name)
	}
	u := sort.Search(len(sf.units), func(i int) bool { return sf.units[i].low > pc }) - 1
	if u >= 0 && pc < sf.units[u].high {
		if lr, err := sf.data.LineReader(sf.units[u].unit); err == nil && lr != nil {
			var le dwarf.LineEntry
			if lr.SeekPC(pc, &le) == nil && le.File != nil {
				out.File = filepath.Base(le.File.Name)
				out.Line = le.Line
			}
		}
	}
	return out, true
}
