# 🎯 **Data Collection Roadmap**

**Goal**: `bingo collect ./main` → "G123 blocked on main.go:42"  
**OS vs Go**: **OS** = Linux kernel (processes/memory). **Go** = runtime (goroutines/channels).

```
Your Go App: goroutines/channels
     ↓
Bingo: Go data (trace) + OS data (memory/locations)
     ↓
JSONL file: "G123 blocked main.go:42"
```

## 🗺️ 4-Step Roadmap (OS vs Go Explained)

### **Step 1: Capture Go Events (GO RUNTIME - 1 day)**
**What**: Tell Go "record everything goroutines do" → `trace.out` file.

**Go Runtime Data**:
```
- Goroutine 123 created
- G123 blocked (chan-send)
- G123 running on CPU core P2
- Timestamps (1.234567s precision)
```

**How** (3 lines!):
```go
// bingo-agent/main.go (our wrapper)
import "runtime/trace"

func main() {
    f, _ := os.Create("trace.out")  // Go data dump
    trace.Start(f)
    defer trace.Stop()
    
    exec.Command(args...).Run()  // Run YOUR app
}
```

**Docker**:
```bash
docker run -v ./app:/app bingo-agent /app/main
→ trace.out created!
```

**✅ Beginner Win**: Go gives you goroutine story for free.

### **Step 2: "Where In Code?" (OS + ELF FILE - 2 days)**
**What**: `trace.out` has addresses like `0x401234`. Convert → `main.go:42`.

**OS Data** (ELF executable):
```
Your compiled main → ELF file (Linux binary format)
├── Machine code: 0x401234 = "movq %rax, (%rcx)"
└── DWARF debug info: 0x401234 = main.go:42
```

**How**:
```
1. Read ELF file bytes (os.ReadFile("main"))
2. Parse DWARF .debug_line table 
3. PC 0x401234 → {File:"main.go", Line:42}
```

**Libraries** (not from scratch):
```go
import (
    "debug/elf"     // OS: Read binary
    "debug/dwarf"   // OS: Parse debug info
)

elfFile, _ := elf.Open("main")
dwarfData, _ := dwarf.Parse(elfFile, ...)
lineReader := dwarfData.LineReader(...)
file, line := lineReader.Line(0x401234)  // main.go:42!
```

**✅ Beginner Win**: stdlib does 90% parsing. Write lookup table.

### **Step 3: Process Memory Map (OS - 1 day)**
**What**: Where does code/data live in RAM? `/proc/PID/maps`.

**OS Data**:
```
/proc/1234/maps:
00400000-00401000 r-xp ... /app/main    ← Code lives here
7f8b1234000-7f8b1238000 rw-p ... [heap] ← Goroutines live here
```

**How**:
```go
func readMaps(pid int) []MemoryRegion {
    data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
    // Parse lines → {Start:0x400000, End:0x401000, File:"/app/main"}
}
```

**Why**: Maps PC addresses to actual files. Docker works (`--pid=host`).

### **Step 4: JSON Events (Glue - 1 day)**
**What**: Combine → beginner-readable file.

**Output** (`events.jsonl`):
```json
{"ts":"1.234567s","g":123,"type":"block","reason":"chan-send","pc":0x401234,"source":{"file":"main.go","line":42}}
{"ts":"1.234800s","g":123,"type":"unblock","reason":"recv"}
```

**How**:
```go
// Step 1 (Go) + Step 2 (OS) → Event → JSON
for event := range parseTrace("trace.out") {
    event.Source = dwarfLookup(event.PC, maps)
    json.NewEncoder(os.Stdout).Encode(event)
}
```

## 📋 **OS vs Go Data Summary**

| Data Source | **OS or Go?** | **Example** | **Tool** |
|-------------|---------------|-------------|----------|
| Goroutine events (create/block) | **GO** | "G123 blocked chan-send" | `runtime/trace.Start()` |
| Code locations (main.go:42) | **OS** | PC 0x401234 → file:line | ELF/DWARF parsing |
| Memory layout | **OS** | `/proc/PID/maps` | Filesystem read |
| Timestamps | **GO** | 1.234567s | `runtime/trace` |

## 🚀 **Complete Beginner Demo**

```
1. $ mkdir bingo-agent && cd bingo-agent
2. $ cat > main.go  # Copy Step 1 code above
3. $ docker build -t bingo-agent .
4. $ docker run -v $(pwd)/app:/app bingo-agent /app/main
5. $ ls → trace.out ✓
6. $ bingo parse trace.out → events.jsonl ✓
7. $ bingo ui events.jsonl → "G123 blocked main.go:42" ✓
```

## 🛠️ **What We Build (Super Simple)**

```
Week 1: bingo-agent (Step 1 - Go trace wrapper)
Week 2: dwarf-lookup (Step 2 - ELF parser)
Week 3: proc-reader (Step 3 - /proc/maps)
Week 4: json-exporter (Step 4 - glue)
```

**Total**: 4 weeks, 1k LOC, beginner-accessible. **No scary ptrace yet** (live debugging = Phase 0 bonus).

**Next**: Write `bingo-agent` Dockerfile? 🎯