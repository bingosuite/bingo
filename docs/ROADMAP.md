# 🚀 **Bingo**: Go Concurrency Debugger - Detailed Technical Roadmap

**Project**: **Bingo** 🎯 - The first Go concurrency debugger that automatically finds deadlocks, goroutine leaks, starvation, and races **in real-time** with click-to-debug workflows.  
**For Go Beginners**: Think "visual gdb but for goroutines" - see live graphs of your concurrent code, click blocked goroutines, jump to exact code line + see variable values.

**Why This Matters**: Go makes concurrent programming easy to *write* (goroutines, channels) but hard to *debug*. Current tools show raw timelines but don't tell you "Goroutine 123 is deadlocked on channel X since 2.3s". Bingo automates this.

## 🎯 What Problem Are We Solving? (Go Concurrency 101)

**Go uses goroutines** (lightweight threads) + **channels/mutexes** for concurrency. Common bugs:
```
func worker(ch chan int) {
    ch <- 42  // DEADLOCK: nobody reads!
}

func main() {
    ch := make(chan int)
    go worker(ch)  // Leak: nobody closes!
    time.Sleep(1s) // Race: unsynchronized access
}
```

**Current debugging sucks**:
```
$ go run -race main.go    # Only finds races, 5x slower
$ go tool trace trace.out # Timeline exists, manual hunting
$ gotraceui trace.out     # Pretty viz, no "this is broken"
```

**Bingo Goal**: `bingo attach <PID>` → live graph → click red node → "DEADLOCK: main.go:5 blocks on ch=0x1234".

## 🔍 Current Tools & Why Bingo Is Different

| Tool | What Beginners See | What It Actually Does | Bingo Does This Better |
|------|-------------------|----------------------|----------------------|
| **`go tool trace`** | Browser timeline of green/yellow bars | Parses `runtime/trace` binary format showing goroutine blocks/scheduling | **Auto-detects** "these 3 bars = deadlock" instead of manual pattern matching |
| **`gotraceui`** | Native app with smooth scrolling timelines | Modern UI for same trace data, 1000x faster than browser | **Adds intelligence** - highlights bugs automatically + click-to-code |
| **`go run -race`** | "WARNING: data race" | Instrumented build detects shared memory races | **Broader scope** - finds deadlocks/leaks/starvation too, no build flag needed |
| **`dlv debug`** | General debugger (breakpoints, step) | ptrace + DWARF parsing for any Go code | **Concurrency-first** - live goroutine graphs instead of thread-by-thread stepping |
| **`delock`** | Logs "DEADLOCK DETECTED" | Mutex wrapper with cycle detection | **No code changes** - reads existing `runtime/trace` + works with channels |

## 🏗️ Core Architecture: Static + Live, Unified Data Model

**Key Insight**: Static analysis (replay crashes) and live debugging (catch bugs as they happen) use the **exact same data structures**. Toggle with `--mode=static` vs `--mode=live`.

```
Raw Data Sources        →      Unified Parser Layer      →      Event Stream        →      Analysis/UI
├── Static Mode:                                                   
│   • trace.out file          │  Custom Go Parsers:          │   Event struct:         │  POST /analyze (JSONL)
│   • ELF binary file         │  • runtime/trace decoder     │   {GID:123, Type:"block",
│   • /proc/PID snapshot      │  • ELF/DWARF reader          │    PC:0x401234, File:"main.go:5"}
│                               │  • procfs maps reader
│
└── Live Mode:
    • ptrace attached process  │  Same parsers!               │   WebSocket /ws/live
    • live trace buffer        │                               │   (delta updates only)
    • /proc/PID live reads
```

**The `Event` struct** - heart of Bingo (same for static/live):
```go
type Event struct {
    Timestamp    time.Time       // When it happened
    GoroutineID  uint64         // G123
    EventType    string         // "create", "block", "unblock", "run", "dead"
    BlockReason  string         // "chan-send", "mutex-lock", "syscall", "GC"
    ProgramCounter uintptr     // 0x401234 - where in machine code
    SourceLocation struct {     // From our ELF parser
        File   string        // "main.go"
        Line   int           // 42
        Function string      // "worker"
    }
    State      string          // "runnable", "running", "blocked"
    // Live-only fields:
    LiveVars   map[string]interface{}  // {"ch": 0x1234, "count": 42}
    Registers  struct { PC, SP uintptr } // Live CPU state
}
```

## 🗺️ Detailed Phased Roadmap

### **Phase 0: Live Attach Foundation** 🎮
**Goal**: `bingo attach 1234` → live WebSocket stream + basic graph  
**For Beginners**: Like `htop` but for goroutines - see them block/leak in real time.

**What We Build**:
```
1. Attach Agent (10KB static Go binary)
   $ bingo attach 1234
   [INFO] PTRACE_ATTACH to PID 1234
   [INFO] Injected trace.Start(buffered=true)
   [INFO] Streaming events over /tmp/bingo-1234.sock
   
2. Live Data Pipeline (100ms heartbeat)
   • Read runtime/trace circular buffer
   • ptrace(PEEKUSER) → live PC/registers for each G
   • /proc/1234/maps → memory layout
   • Send JSONL deltas: {"G123": {"state":"blocked", "PC":0x401234}}
   
3. WebSocket Server
   ws://localhost:8080/ws/live/1234
   → Graph updates every 100ms
```

**Demo Flow**:
```
$ docker run -d --cap-add=SYS_PTRACE myapp:latest
$ bingo attach $(docker pid myapp)
[Live graph opens] → See goroutines turn red → Click → "DEADLOCK DETECTED"
```

**Success Criteria**:
- 10Hz updates (<100ms latency)
- Graph shows live blocking (G123 → "chan-send")
- Click G → jumps to `main.go:42`
- <5% CPU overhead on 10k goroutines

### **Phase 1: Unified Data Foundation** 🧱
**Goal**: Same parsers work for **both** `bingo collect ./main` (static) **and** live attach.

**Static Mode** (`bingo collect ./main --mode=static`):
```
1. docker run -v ./app bingo-entrypoint ./main
2. Automatically calls: runtime/trace.Start(), run binary, trace.Stop()
3. Parse trace.out → events.jsonl
4. bingo ui events.jsonl  # Scrubbable timeline
```

**Live Mode** (`bingo attach <PID> --mode=live`):
```
1. ptrace(ATTACH) → pause process
2. Inject minimal agent (write to process memory)
3. Resume → agent streams trace buffer
4. Same parsers as static mode!
```

**Core Parsers We Write** (80% shared code):
| Parser | Input | Output | Static | Live |
|--------|-------|--------|--------|------|
| **Trace Parser** | `runtime/trace` binary | Event structs | trace.out file | Live buffer |
| **ELF/DWARF** | Go binary | PC→file:line | Binary on disk | Same (mmap) |
| **Proc Reader** | `/proc/PID/maps` | Memory layout | Snapshot | Live reads |
| **Frame Walker** | Stack + DWARF | Local vars | Core dump | ptrace readmem |

**Demo**: Same `events.jsonl` from static file **or** 30s live capture.

### **Phase 2: Smart Analysis Engine & API** 🧠
**Goal**: Turn raw events → "DEADLOCK: Goroutines 123,456 on channel 0x1234"

**Detectors** (rolling window for live, full trace for static):

| Bug Type | How We Detect | Beginner-Friendly Output |
|----------|---------------|-------------------------|
| **Deadlock** | Build block graph (G123 blocks on chan X, G456 blocks on G123) → Tarjan cycle detection | `"Goroutines 123→456 deadlocked 2.3s. Kill G123?"` |
| **Leak** | Goroutine count > 1000 after 10s + no progress | `"Leak: 1,247 goroutines from main.go:42 still alive"` |
| **Starvation** | P0 idle but 50+ runnable goroutines | `"Starvation: Worker pool hung on main.go:15"` |
| **Race Hint** | Same PC accessed by multiple unsynchronized Gs | `"Potential race: main.go:23 accessed by 3 goroutines"` |

**API Layer** (stateless, works for both modes):
```
POST /analyze          # Static: upload JSONL → bug report
WS /live/analyze/1234  # Live: rolling analysis every second

GET /goroutines/123    # G123 timeline + current state
GET /graph             # Current block graph (JSON for D3.js)
```

### **Phase 3: Click-to-Debug UIs** 🎨
**Goal**: Click graph node → editor jumps to code + shows vars.

| UI Type | Beginner Features | Power User Features |
|---------|------------------|-------------------|
| **VSCode Extension** | `"Bingo: Attach Live"` command<br>Live graph sidebar<br>Click → editor highlight | Var inspection tree<br>Timeline scrubber<br>Breakpoint on blocked Gs |
| **TUI** (`bingo ui`) | `htop`-style live view<br>Arrow keys navigate graph | Regex filter (`blocked.*chan`)<br>Export flamegraph |
| **Web Dashboard** | D3 force-directed graph<br>Real-time highlights | Session recording<br>Team sharing |

**Click Flow** (works static **and** live):
```
1. Graph node glows red (blocked >1s)
2. Click → API `/goroutines/123/inspect`
3. Response: {PC:0x401234, Vars:{"ch":0x1234, "count":42}}
4. ELF parser: 0x401234 → main.go:42
5. VSCode: Open file + highlight line 42 + sidebar "ch=[chan int] (blocked)"
```

## 🔧 Technical Implementation Details

**Phase 0 Live Agent** (injected into target):
```go
// 200 lines total
func main() {
    runtime/trace.Start(buffered=1024*1024)  // 1MB circular buffer
    ticker := time.NewTicker(100 * time.Millisecond)
    for range ticker.C {
        events := pollTraceBuffer()
        streamToUnixSocket("/tmp/bingo.sock", events)
    }
}
```

**Ptrace Integration** (Go `syscall` package):
```go
// Attach to live process
syscall.PtraceAttach(pid)
syscall.PtraceSetOptions(pid, PTRACE_O_TRACEEXEC)

// Read goroutine 123's registers
regs := syscall.PtraceGetRegs(pid)
pc := uintptr(regs.Rip)  // Live PC → our ELF parser
```

**Docker Entrypoint** (Phase 1 static):
```bash
#!/bin/sh
exec bingo-agent --mode=static --output=/output/trace.out -- "$@"
```

## 🚀 Usage Examples (Copy-Paste Ready)

**Development**:
```bash
# Live debugging
bingo attach $(pgrep myapp)  # Opens VSCode graph

# Static analysis (CI)
bingo collect ./main --output=events.jsonl
curl -F file=@events.jsonl https://bingo.example.com/analyze
```

**Production Docker**:
```dockerfile
# Dockerfile
RUN apk add --no-cache libcap  # For SYS_PTRACE
RUN setcap cap_sys_ptrace=ep ./myapp

# Run
docker run --cap-add=SYS_PTRACE -p 8080:8080 myapp
bingo attach $(docker pid myapp)
```

## 📋 Success Criteria Per Phase

| Phase | Green Checkmarks |
|-------|------------------|
| **Phase 0** | `bingo attach` → live graph updates<br>Click G → shows current file:line |
| **Phase 1** | Same JSONL from static **and** live<br>PC 0x401234 → main.go:42 |
| **Phase 2** | `curl /analyze` → `{"bugs":[{"type":"deadlock",...}]}`<br>Detects GoBench bugs |
| **Phase 3** | VSCode: click graph → editor jumps<br>TUI: arrow keys navigate blocks |