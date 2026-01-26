# Existing Solutions Analysis

This document surveys current tools for Go concurrency debugging. **Bingo differentiates** by providing **automated bug detection + live click-to-debug graphs** across static/live modes, without code changes or external binaries.

## 1. Execution Trace Visualizers

### `go tool trace` (Official Go Toolchain)
**Description**: Built-in CLI that parses `runtime/trace` files and launches a browser-based viewer showing goroutine timelines, scheduler activity, GC phases, syscalls, and network events. [tip.golang](https://tip.golang.org/src/runtime/trace/trace.go)

**How to Use** (for beginners):
```bash
# In your Go program:
import "runtime/trace"
f, _ := os.Create("trace.out")
trace.Start(f)
defer trace.Stop()  // Add to main()

# Generate trace:
go run -trace=trace.out main.go

# View:
go tool trace trace.out  # Opens browser
```

**Strengths**:
- Official, zero install
- Shows goroutine create/block/unblock/run/death
- Processor (P) scheduling, GC stops, heap growth
- Search/filter by goroutine ID or duration

**Limitations**:
- Browser-based (slow for million-event traces, deprecated APIs)
- **Manual analysis only** - no automatic deadlock/race/leak detection
- Static files only (no live attach)
- Overwhelming for beginners (dense timelines, no "this is broken" hints)
- **Links**: [Go Docs](https://pkg.go.dev/cmd/trace), [Tutorial](https://leapcell.io/blog/unveiling-go-program-behavior-with-go-tool-trace)

### **Gotraceui** (Third-Party Native UI)
**Description**: Modern native desktop app for `runtime/trace` visualization. Created by dominikh (golangci-lint author). Faster/more powerful than `go tool trace`. 1.3k GitHub stars, MIT license. [github](https://github.com/dominikh/gotraceui)

**Install & Use**:
```bash
go install honnef.co/go/gotraceui/cmd/gotraceui@latest
gotraceui trace.out
```

**Features** (from README/manual):
- **Performant**: Handles millions of events in real-time (no browser lag)
- **Go-specific**: Understands goroutine relationships, GC interactions
- Flame graphs, resizable/sortable tables, intelligent search/filter
- Cross-platform (Linux/macOS/Windows)
- Exhaustive manual explaining Go runtime + traces
- Recent: Go 1.21+, v1.22 trace support [github](https://github.com/dominikh/gotraceui/issues/159)

**Strengths**:
- Native Gio UI (smooth scrolling, no WebGL issues)
- Beginner-friendly manual + sponsorship model
- **Roadmap**: More analysis features planned
- **Links**: [Site](https://gotraceui.dev), [GitHub](https://github.com/dominikh/gotraceui), [Manual](https://gotraceui.dev/manual/latest/), [Releases](https://github.com/dominikh/gotraceui/releases)

**Limitations**:
- **View-only** - no automated bug detection
- Desktop app only (no API/embed/VSCode integration)
- Static traces (no live debugging)
- Requires manual pattern recognition for deadlocks/etc.

## 2. General Debuggers

### **Delve (dlv)** (Official Go Debugger)
**Description**: Full-featured debugger for Go (ptrace + DWARF parsing). Supports breakpoints, stepping, variable inspection, headless RPC. 25k+ GitHub stars. [golang](https://golang.cafe/blog/golang-debugging-with-delve)

**Modes**:
```bash
dlv debug main.go           # Compile + debug
dlv attach 1234             # Attach live PID
dlv --headless --listen=:0  # RPC for VSCode
```

**Key Commands** (from docs):
```
(dlv) list main.go:10       # Show source
(dlv) break main.go:10      # Breakpoint
(dlv) continue              # Run to breakpoint
(dlv) locals                # Show variables
(dlv) goroutines            # List Gs
(dlv) print ch              # Inspect channel
```

**Strengths**:
- Live attach (`dlv-attach`)
- VSCode integration (Debug Adapter Protocol)
- Core dumps, DAP, trace mode
- **Links**: [GitHub](https://github.com/go-delve/delve), [Usage](https://golang.cafe/blog/golang-debugging-with-delve), [Attach Docs](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_attach.md)

**Limitations**:
- **Generic debugger** - not concurrency-optimized
- Manual goroutine switching (`goroutine 123`)
- No trace timelines or automatic deadlock graphs
- Heavy binary (~50MB), ptrace permissions tricky in Docker

## 3. Specialized Concurrency Tools

### **Race Detector** (`go run -race`)
**Description**: Official dynamic race detector. Instruments memory accesses at compile-time. [go](https://go.dev/blog/race-detector)

**Use**:
```bash
go build -race
go run -race main.go
# Output: WARNING: DATA RACE ... main.go:10 +0x00
```

**Strengths**:
- Official, detects shared-memory races
- Stack traces for conflicting accesses
- `GORACE` env for tuning

**Limitations**:
- **Races only** (misses deadlocks, leaks, starvation)
- 5-10x slowdown
- Build flag required
- **Links**: [Go Blog](https://go.dev/blog/race-detector), [Docs](https://go.dev/doc/articles/race_detector)

### **Deadlock Detectors** (Runtime Wrappers)
| Library | Description | Links |
|---------|-------------|-------|
| **`delock`** | `sync.Mutex` wrapper w/ timeout + stack traces. Groups deadlocks by lock type. | [GitHub](https://github.com/ietxaniz/delock), [Article](https://ietxaniz.github.io/go-deadlock-detection-delock-library)  [github](https://github.com/ietxaniz/delock) |
| **`go-deadlock`** | Mutex monitor w/ cycle detection. | [GitHub](https://github.com/sasha-s/go-deadlock)  [pkg.go](https://pkg.go.dev/github.com/sasha-s/go-deadlock) |
| **`Deadlock-Go`** | Online deadlock detection. | [GitHub](https://github.com/ErikKassubek/Deadlock-Go)  [github](https://github.com/ErikKassubek/Deadlock-Go) |

**Pattern**: Replace `sync.Mutex` → `delock.Mutex`. Logs cycles but:
- **Code changes everywhere**
- Runtime overhead
- **Mutex-only** (misses channels, sync.Cond)
- No visualization/API

## 4. Other/Notable Mentions

| Tool | Category | Notes | Link |
|------|----------|-------|------|
| **pprof** | Profiling | Goroutine profiles (not event traces). Aggregates only. | [pkg.go.dev/net/http/pprof](https://pkg.go.dev/net/http/pprof) |
| **xgotop** | Live viz | Real-time runtime dashboard (CPU/Mem/Gs). | [Reddit](https://reddit.com/r/golang/comments/1pyw43j)  [reddit](https://www.reddit.com/r/golang/comments/1pyw43j/xgotop_realtime_go_runtime_visualization/) |
| **eBPF Tools** | Kernel tracing | Goroutine state tracing (advanced). | [Tutorial](https://eunomia.dev/tutorials/31-goroutine)  [eunomia](https://eunomia.dev/tutorials/31-goroutine/) |
| **GOAT** | Research | Automated concurrency analysis. Not prod-ready. | [Paper](https://staheri.github.io/files/iiswc21-paper7.pdf)  [staheri.github](https://staheri.github.io/files/iiswc21-paper7.pdf) |

## 🎯 Bingo vs Ecosystem Summary Table

| Feature | go tool trace | Gotraceui | Delve | Race | delock | **Bingo** |
|---------|---------------|-----------|-------|------|--------|-----------|
| Live Attach | ❌ | ❌ | ✅ | ❌ | ❌ | ✅ |
| Auto Bug Detection | ❌ | ❌ | ❌ | Races only | Mutex only | ✅ All types |
| Goroutine Graphs | Timeline | Timeline | List | ❌ | ❌ | ✅ Interactive |
| Click-to-Code | ❌ | File links | ✅ | ❌ | ❌ | ✅ + Vars |
| Docker-Native | Manual | Manual | Tricky | Build flag | Code change | ✅ Entrypoint |
| VSCode Integration | ❌ | ❌ | ✅ | ❌ | ❌ | ✅ Live graph |
| No Code Changes | ✅ | ✅ | ✅ | ❌ | ❌ | ✅ |

**Bingo Gap Fill**: **The missing "concurrency Sentry"** - automated, live, visual, zero-config.

**Sources**: GitHub repos, Go docs, blog posts [gotraceui](https://gotraceui.dev)