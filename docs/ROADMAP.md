# Bingo Roadmap

This roadmap reflects the current project direction and delivery order.

## Phase 1: Basic Debugger Implementation

Goal: deliver a working debugger foundation for Go programs.

Scope:

- Build the core debugger loop (start, attach, control, stop).
- Implement process inspection and basic runtime state capture.
- Add platform-specific support currently targeted by the codebase (darwin arm64 and linux amd64).
- Expose core debugger behavior through stable internal interfaces.
- Provide baseline tests for debugger lifecycle and core functionality.

Success criteria:

- Can start or attach to a target program reliably.
- Can collect and report basic execution state without crashing.
- Core debugger tests pass in CI for supported platforms.

## Phase 2: Concurrency Features

Goal: extend the debugger to understand and surface Go concurrency behavior.

Scope:

- Goroutine tracking (lifecycle, state, and transitions).
- Channel activity visibility (send, receive, blocking points).
- Mutex and lock-related visibility (lock/unlock/wait contention).
- Additional synchronization insights where feasible (for example wait groups).
- Improve analysis and test coverage for concurrent scenarios.

Success criteria:

- Goroutine-level state and transitions are available in debugger output.
- Blocking and waiting on channels/mutexes can be identified.
- Concurrency-focused tests validate core tracking behavior.

## Phase 3: Client Implementation

Goal: provide a usable client experience on top of the debugger engine.

Scope:

- Implement the Bingo client entrypoint and connection flow.
- Integrate with websocket/protocol components for live debugger interaction.
- Add user-facing commands and output for inspecting debugger/concurrency state.
- Improve reliability, error handling, and user guidance.
- Add integration tests for end-to-end client-debugger workflows.

Success criteria:

- Client can connect to the debugger and display useful runtime/concurrency data.
- Typical user workflows complete successfully end-to-end.
- Integration tests cover critical client scenarios.

## Milestone Notes

- Phase order is sequential by design: each phase builds on the previous one.
- Scope inside a phase can be delivered incrementally, but phase goals should remain unchanged.
- The roadmap can be revised again if project priorities shift.
