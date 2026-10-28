package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Crash and hang reports from SnagPostmortem carry the raw MetricKit diagnostic as
// diagnostic.json. With the app's dSYMs uploaded, the server symbolicates its frames.

// Symbolication records what the server could resolve for a report.
type Symbolication struct {
	Resolved int         `json:"resolved"`
	Frames   int         `json:"frames"`
	Missing  []SymbolRef `json:"missing,omitempty"` // app binaries without uploaded symbols
	At       time.Time   `json:"at"`
}

type SymbolRef struct {
	UUID   string `json:"uuid"`
	Binary string `json:"binary"`
}

const (
	diagnosticFilename   = "diagnostic.json"
	symbolicatedFilename = "symbolicated.txt"
	symbolicatedName     = "symbolicated.txt" // on disk
)

type mxDiagnostic struct {
	CallStackTree struct {
		CallStacks []struct {
			ThreadAttributed bool      `json:"threadAttributed"`
			RootFrames       []mxFrame `json:"callStackRootFrames"`
		} `json:"callStacks"`
	} `json:"callStackTree"`
}

type mxFrame struct {
	BinaryUUID  string    `json:"binaryUUID"`
	BinaryName  string    `json:"binaryName"`
	Offset      flexUint  `json:"offsetIntoBinaryTextSegment"`
	Address     flexUint  `json:"address"`
	SampleCount flexUint  `json:"sampleCount"`
	SubFrames   []mxFrame `json:"subFrames"`
}

// flexUint accepts MetricKit's numbers, which are sometimes written as floats or strings.
type flexUint uint64

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	if n, err := strconv.ParseUint(s, 0, 64); err == nil {
		*f = flexUint(n)
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v >= 1.8e19 {
		return nil
	}
	*f = flexUint(v)
	return nil
}

type mxThread struct {
	attributed bool
	frames     []mxFrame
}

// threads flattens the call tree: the first chain for crashes, the heaviest path for hangs.
func (d *mxDiagnostic) threads(hang bool) []mxThread {
	var out []mxThread
	for _, cs := range d.CallStackTree.CallStacks {
		var frames []mxFrame
		level := cs.RootFrames
		for len(level) > 0 && len(frames) < 2048 {
			next := level[0]
			if hang {
				for _, f := range level[1:] {
					if f.SampleCount > next.SampleCount {
						next = f
					}
				}
			}
			frames = append(frames, next)
			level = next.SubFrames
		}
		out = append(out, mxThread{attributed: cs.ThreadAttributed, frames: frames})
	}
	return out
}

var systemBinaries = map[string]bool{
	"UIKitCore": true, "CoreFoundation": true, "Foundation": true, "SwiftUI": true, "SwiftUICore": true,
	"AttributeGraph": true, "GraphicsServices": true, "dyld": true, "QuartzCore": true, "UIKit": true,
	"AppKit": true, "CFNetwork": true, "Combine": true, "CoreData": true, "WebKit": true, "AVFCore": true,
}

func looksLikeAppBinary(name string) bool {
	return name != "" && !strings.HasPrefix(name, "lib") && !systemBinaries[name]
}

type symbolicated struct {
	info Symbolication
	text string
	top  string // the first resolved frame of the crashed thread, for the title
}

// symbolicateDiagnostic resolves a MetricKit diagnostic against a project's symbols.
func (s *Server) symbolicateDiagnostic(project string, data []byte, hang bool) (*symbolicated, error) {
	var d mxDiagnostic
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	threads := d.threads(hang)
	if len(threads) == 0 {
		return nil, nil
	}
	out := &symbolicated{info: Symbolication{At: time.Now().UTC()}}
	missing := map[string]SymbolRef{}
	var b strings.Builder
	crashed := 0
	for i, t := range threads {
		if t.attributed {
			crashed = i
			break
		}
	}
	for i, t := range threads {
		label := "Thread %d"
		if i == crashed {
			if hang {
				label += " (hung, heaviest path)"
			} else {
				label += " (crashed)"
			}
		}
		fmt.Fprintf(&b, label+"\n", i)
		for n, f := range t.frames {
			out.info.Frames++
			binary := f.BinaryName
			if binary == "" {
				binary = "???"
			}
			line := fmt.Sprintf("%-3d %-28s +0x%x", n, binary, uint64(f.Offset))
			uuid := normalizeUUID(f.BinaryUUID)
			if ValidID(uuid) && s.symbols.Has(project, uuid) {
				if sym, ok := s.symbols.Lookup(project, uuid, uint64(f.Offset), !(n == 0 && !hang)); ok {
					out.info.Resolved++
					line = fmt.Sprintf("%-3d %-28s %s", n, binary, sym.Function)
					if sym.File != "" && sym.Line > 0 {
						line += fmt.Sprintf(" (%s:%d)", sym.File, sym.Line)
					}
					for _, outer := range sym.InlinedInto {
						line += "\n    " + strings.Repeat(" ", 28) + "inlined into " + outer
					}
					if i == crashed && out.top == "" {
						out.top = sym.Function
					}
				}
			} else if ValidID(uuid) && looksLikeAppBinary(f.BinaryName) {
				missing[uuid] = SymbolRef{UUID: uuid, Binary: f.BinaryName}
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	for _, ref := range missing {
		out.info.Missing = append(out.info.Missing, ref)
	}
	sortSymbolRefs(out.info.Missing)
	header := fmt.Sprintf("Symbolicated by snag-server: %d of %d frames resolved.\n", out.info.Resolved, out.info.Frames)
	for _, ref := range out.info.Missing {
		header += fmt.Sprintf("No symbols uploaded for %s (%s).\n", ref.Binary, ref.UUID)
	}
	out.text = header + "\n" + b.String()
	return out, nil
}

func sortSymbolRefs(refs []SymbolRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j].Binary+refs[j].UUID < refs[j-1].Binary+refs[j-1].UUID; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

// The client's titles and summaries fall back to "Binary +0x1a2b" when it couldn't resolve the
// top app frame on device (stripped release builds).
var rawFrameSuffix = regexp.MustCompile(` in (\S+) \+0x[0-9a-fA-F]+$`)
var rawTopFrameLine = regexp.MustCompile(`(?m)^Top app frame: \S+ \+0x[0-9a-fA-F]+$`)

// symbolicateIncoming runs at ingest, before the report is filed, so the resolved function
// name goes into the title and identical crashes group across builds.
func (s *Server) symbolicateIncoming(project string, rec *Record, dir string) {
	diag := -1
	for i, f := range rec.Files {
		if f.Filename == diagnosticFilename {
			diag = i
		}
	}
	if diag < 0 {
		return
	}
	data, err := os.ReadFile(filepath.Join(dir, rec.Files[diag].Name))
	if err != nil {
		return
	}
	result, err := s.symbolicateDiagnostic(project, data, rec.Kind == "hang")
	if err != nil || result == nil {
		return
	}
	rec.Symbolication = &result.info
	if result.info.Resolved == 0 {
		return
	}
	if result.top != "" {
		if rawFrameSuffix.MatchString(rec.Title) {
			rec.Title = rawFrameSuffix.ReplaceAllString(rec.Title, " in "+result.top)
		}
		rec.Message = rawTopFrameLine.ReplaceAllString(rec.Message, "Top app frame: "+result.top)
	}
	if err := os.WriteFile(filepath.Join(dir, symbolicatedName), []byte(result.text), 0o640); err != nil {
		return
	}
	file := StoredFile{Name: symbolicatedName, Filename: symbolicatedFilename, Role: "log", ContentType: "text/plain; charset=utf-8", Size: int64(len(result.text))}
	rec.Files = append([]StoredFile{file}, rec.Files...)
}

// resymbolicate updates stored reports of a project that were missing any of uuids. Titles and
// grouping stay as they were; the symbolicated log is added or replaced.
func (s *Server) resymbolicate(project string, uuids []string) int {
	want := map[string]bool{}
	for _, u := range uuids {
		want[normalizeUUID(u)] = true
	}
	n := 0
	for _, rec := range s.store.All() {
		if rec.Project != project || rec.Symbolication == nil {
			continue
		}
		hit := false
		for _, m := range rec.Symbolication.Missing {
			hit = hit || want[m.UUID]
		}
		if !hit {
			continue
		}
		var diag *StoredFile
		for i := range rec.Files {
			if rec.Files[i].Filename == diagnosticFilename {
				diag = &rec.Files[i]
			}
		}
		if diag == nil {
			continue
		}
		path, _, err := s.store.FilePath(rec.ID, diag.Name)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		result, err := s.symbolicateDiagnostic(project, data, rec.Kind == "hang")
		if err != nil || result == nil {
			continue
		}
		if result.info.Resolved > 0 {
			if err := s.store.PutFile(rec.ID, StoredFile{Name: symbolicatedName, Filename: symbolicatedFilename, Role: "log", ContentType: "text/plain; charset=utf-8"}, []byte(result.text)); err != nil {
				s.log.Error("saving symbolicated log", "report", rec.ID, "err", err)
				continue
			}
		}
		info := result.info
		_, _ = s.store.Update(rec.ID, func(r *Record) { r.Symbolication = &info })
		n++
	}
	return n
}
