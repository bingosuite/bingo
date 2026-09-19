package dap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func fakeSourceArtifact(t *testing.T) *sourceArtifact {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), "owned-build-")
	if err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "debuggee")
	if err := os.WriteFile(program, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &sourceArtifact{dir: dir, program: program}
}

func sourceRequest(t *testing.T, dir string) *godap.LaunchRequest {
	t.Helper()
	args, err := json.Marshal(launchConfig{
		Mode: "debug", Program: dir, StopOnEntry: true,
		Args: []string{"argument"}, Env: []string{"SOURCE_TEST=value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &godap.LaunchRequest{Arguments: args}
}

func newSourceHarness(t *testing.T, builder func(context.Context, launchConfig) (*sourceArtifact, error)) *harness {
	t.Helper()
	rec := &cmdRecorder{}
	sess := &fakeSession{id: "source-session", cmds: rec, done: make(chan struct{})}
	hh := newHarnessProvider(t, &fakeProvider{sess: sess}, rec, func(h *Handler) { h.buildSource = builder })
	t.Cleanup(func() {
		_ = hh.handler.Close()
		hh.handler.builds.Wait()
		close(sess.done)
		hh.handler.artifacts.wg.Wait()
	})
	return hh
}

func awaitSourceSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("source lifecycle operation did not finish")
	}
}

func requireArtifact(t *testing.T, artifact *sourceArtifact, exists bool) {
	t.Helper()
	_, err := os.Stat(artifact.program)
	if (exists && err != nil) || (!exists && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("artifact exists=%t: %v", exists, err)
	}
}

func TestSourceBuildRemainsResponsiveAndRejectsLateSuccess(t *testing.T) {
	artifact := fakeSourceArtifact(t)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	deadlines := make(chan time.Duration, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	hh := newSourceHarness(t, func(ctx context.Context, _ launchConfig) (*sourceArtifact, error) {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if ok {
			deadlines <- time.Until(deadline)
		} else {
			deadlines <- 0
		}
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return artifact, nil
	})
	dir := t.TempDir()
	launchSeq := hh.sendReq("launch", sourceRequest(t, dir))
	awaitSourceSignal(t, started)
	if remaining := <-deadlines; remaining <= 0 || remaining > sourceBuildTimeout {
		t.Fatalf("live build deadline = %s, want a positive bound <= %s", remaining, sourceBuildTimeout)
	}
	hh.handler.mu.Lock()
	announced := hh.handler.sessionAnnounced
	hh.handler.mu.Unlock()
	if announced {
		t.Fatal("source session announced before compilation")
	}

	for _, command := range []string{"launch", "attach", "restart", "configurationDone"} {
		var request godap.RequestMessage
		switch command {
		case "launch":
			request = sourceRequest(t, dir)
		case "attach":
			request = &godap.AttachRequest{Arguments: json.RawMessage(`{"session":"another-session"}`)}
		case "restart":
			request = &godap.RestartRequest{}
		case "configurationDone":
			request = &godap.ConfigurationDoneRequest{}
		}
		seq := hh.sendReq(command, request)
		response := recvType[*godap.ErrorResponse](hh)
		if response.RequestSeq != seq || response.Command != command {
			t.Fatalf("wrong pending request settled: %+v", response)
		}
	}
	for range 2 {
		seq := hh.sendReq("terminate", &godap.TerminateRequest{})
		if got := recvType[*godap.TerminateResponse](hh); got.RequestSeq != seq {
			t.Fatalf("terminate acknowledgement = %+v", got)
		}
	}
	awaitSourceSignal(t, canceled)
	if hh.cmds.count(protocol.CmdLaunch) != 0 || hh.cmds.count(protocol.CmdKill) != 0 {
		t.Fatal("compile cancellation submitted run control")
	}
	once.Do(func() { close(release) })
	failure := recvType[*godap.ErrorResponse](hh)
	if failure.RequestSeq != launchSeq || failure.Command != "launch" {
		t.Fatalf("original launch was not settled: %+v", failure)
	}
	_ = recvType[*godap.TerminatedEvent](hh)
	hh.handler.builds.Wait()
	requireArtifact(t, artifact, false)
	if calls.Load() != 1 || hh.cmds.count(protocol.CmdLaunch) != 0 {
		t.Fatal("duplicate or canceled compile launched a target")
	}
	if _, err := godap.ReadBaseMessage(hh.reader); !errors.Is(err, io.EOF) {
		t.Fatalf("startup emitted extra messages after its terminal: %v", err)
	}
}

func TestSourceDisconnectCancelsBeforeLaunch(t *testing.T) {
	artifact := fakeSourceArtifact(t)
	started := make(chan struct{})
	hh := newSourceHarness(t, func(ctx context.Context, _ launchConfig) (*sourceArtifact, error) {
		close(started)
		<-ctx.Done()
		return artifact, nil
	})
	hh.sendReq("launch", sourceRequest(t, t.TempDir()))
	awaitSourceSignal(t, started)
	seq := hh.sendReq("disconnect", &godap.DisconnectRequest{})
	if response := recvType[*godap.DisconnectResponse](hh); response.RequestSeq != seq {
		t.Fatalf("disconnect acknowledgement = %+v", response)
	}
	hh.handler.builds.Wait()
	requireArtifact(t, artifact, false)
	if hh.cmds.count(protocol.CmdLaunch) != 0 {
		t.Fatal("disconnected build launched a target")
	}
}

func TestSourceTerminateAcknowledgementPrecedesBuildFailure(t *testing.T) {
	started := make(chan struct{})
	hh := newSourceHarness(t, func(ctx context.Context, _ launchConfig) (*sourceArtifact, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	launchSeq := hh.sendReq("launch", sourceRequest(t, t.TempDir()))
	awaitSourceSignal(t, started)
	terminateSeq := hh.sendReq("terminate", &godap.TerminateRequest{})
	ack, ok := hh.recv().(*godap.TerminateResponse)
	if !ok || ack.RequestSeq != terminateSeq {
		t.Fatal("immediate cancellation completion overtook its acknowledgement")
	}
	failure, ok := hh.recv().(*godap.ErrorResponse)
	if !ok || failure.RequestSeq != launchSeq {
		t.Fatal("canceled launch was not settled after terminate acknowledgement")
	}
	if _, ok := hh.recv().(*godap.TerminatedEvent); !ok {
		t.Fatal("canceled source launch did not terminate")
	}
}

func TestFailedSourceBuildRetiresEmptySession(t *testing.T) {
	for _, cause := range []error{errors.New("compiler failed"), context.DeadlineExceeded, errors.New("Go build requires 'go' on the server PATH")} {
		t.Run(cause.Error(), func(t *testing.T) {
			var factories atomic.Int32
			log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
			hb := hub.NewSession("failed-build", func() debugger.Debugger {
				factories.Add(1)
				return newRaceDebugger()
			}, log)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go hb.Run(ctx)
			hh := newHarnessProvider(t, &hubProvider{session: hb}, &cmdRecorder{}, func(h *Handler) {
				h.buildSource = func(context.Context, launchConfig) (*sourceArtifact, error) { return nil, cause }
			})
			seq := hh.sendReq("launch", sourceRequest(t, t.TempDir()))
			response := hh.recv()
			failure, ok := response.(*godap.ErrorResponse)
			if !ok || failure.RequestSeq != seq || failure.Command != "launch" {
				t.Fatalf("build failure must precede discovery or initialization: %T %+v", response, response)
			}
			_ = recvType[*godap.TerminatedEvent](hh)
			awaitSourceSignal(t, hb.Done())
			if factories.Load() != 0 {
				t.Fatal("failed build constructed a debugger")
			}
		})
	}
}

func TestServerCloseCancelsAndJoinsSourceBuild(t *testing.T) {
	artifact := fakeSourceArtifact(t)
	canceled, release := make(chan struct{}), make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s := quietServer(t)
	s.provider.(*fakeProvider).sess.done = make(chan struct{})
	s.buildSource = func(ctx context.Context, _ launchConfig) (*sourceArtifact, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return artifact, nil
	}
	address, err := s.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp4", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := sourceRequest(t, t.TempDir())
	request.Seq, request.Type, request.Command = 1, "request", "launch"
	if err := godap.WriteProtocolMessage(conn, request); err != nil {
		t.Fatal(err)
	}
	awaitSourceSignal(t, started)
	closed := make(chan struct{})
	go func() { _ = s.Close(); close(closed) }()
	awaitSourceSignal(t, canceled)
	select {
	case <-closed:
		t.Fatal("server reported closure before its compiler returned")
	default:
	}
	requireArtifact(t, artifact, true)
	once.Do(func() { close(release) })
	awaitSourceSignal(t, closed)
	requireArtifact(t, artifact, false)
}

type sourceLaunchDebugger struct {
	*raceDebugger
	launches    chan protocol.LaunchPayload
	entryGate   <-chan struct{}
	cleanupGate <-chan struct{}
	cleanupSeen chan struct{}
	cleanupOnce sync.Once
}

func (d *sourceLaunchDebugger) LaunchWithOptions(program string, args, env []string, options debugger.LaunchOptions) error {
	if _, err := os.Stat(program); err != nil {
		return err
	}
	d.launches <- protocol.LaunchPayload{Program: program, Args: args, Env: env, Cwd: options.Cwd}
	if d.entryGate != nil {
		<-d.entryGate
	}
	return d.raceDebugger.Launch(program, args, env)
}

func (d *sourceLaunchDebugger) Kill() error {
	if d.cleanupGate != nil {
		select {
		case <-d.cleanupGate:
		default:
			d.cleanupOnce.Do(func() { close(d.cleanupSeen) })
			return debugger.ErrBackendCleanupIncomplete
		}
	}
	return d.raceDebugger.Kill()
}

func TestSourceArtifactsFollowHubLifetimeAcrossRestartAndObservers(t *testing.T) {
	artifact := fakeSourceArtifact(t)
	entryGate, cleanupGate, cleanupSeen := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var entryOnce, cleanupOnce sync.Once
	defer entryOnce.Do(func() { close(entryGate) })
	defer cleanupOnce.Do(func() { close(cleanupGate) })
	launches := make(chan protocol.LaunchPayload, 3)
	var created atomic.Int32
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	hb := hub.NewSession("source-lifetime", func() debugger.Debugger {
		d := &sourceLaunchDebugger{raceDebugger: newRaceDebugger(), launches: launches}
		switch created.Add(1) {
		case 1:
			d.entryGate = entryGate
		case 3:
			d.cleanupGate, d.cleanupSeen = cleanupGate, cleanupSeen
		}
		return d
	}, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.Run(ctx)
	var builds atomic.Int32
	hh := newHarnessProvider(t, &hubProvider{session: hb}, &cmdRecorder{}, func(h *Handler) {
		h.buildSource = func(context.Context, launchConfig) (*sourceArtifact, error) {
			builds.Add(1)
			return artifact, nil
		}
	})
	t.Cleanup(func() {
		entryOnce.Do(func() { close(entryGate) })
		cleanupOnce.Do(func() { close(cleanupGate) })
		cancel()
		awaitSourceSignal(t, hb.Done())
		hh.handler.builds.Wait()
		hh.handler.artifacts.wg.Wait()
	})
	dir := t.TempDir()
	launchSeq := hh.sendReq("launch", sourceRequest(t, dir))
	first := receiveSourceLaunch(t, launches)
	hh.handler.mu.Lock()
	announced := hh.handler.sessionAnnounced
	hh.handler.mu.Unlock()
	if announced {
		t.Fatal("source discovery preceded successful native entry")
	}
	requireArtifact(t, artifact, true)
	entryOnce.Do(func() { close(entryGate) })
	event, ok := hh.recv().(*sessionEvent)
	if !ok || event.Body.SessionID != hb.SessionID() {
		t.Fatal("source discovery must precede initialized")
	}
	obs := newObserver()
	defer obs.Close()
	if _, err := hb.AddClient(obs, log); err != nil {
		t.Fatal(err)
	}
	obs.send(t, protocol.CmdGoroutineSnapshot, nil)
	obs.waitEvent(t, protocol.EventGoroutineSnapshot, nil)
	_ = recvType[*godap.InitializedEvent](hh)
	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	_ = recvType[*godap.ConfigurationDoneResponse](hh)
	if response := recvType[*godap.LaunchResponse](hh); response.RequestSeq != launchSeq {
		t.Fatalf("source launch response = %+v", response)
	}
	_ = recvType[*godap.StoppedEvent](hh)
	hh.sendReq("restart", &godap.RestartRequest{})
	second := receiveSourceLaunch(t, launches)
	_ = recvType[*godap.RestartResponse](hh)
	_ = recvType[*godap.StoppedEvent](hh)
	if first.Program != artifact.program || first.Cwd != dir || !reflect.DeepEqual(first, second) {
		t.Fatalf("restart lost source launch options: first=%+v second=%+v", first, second)
	}
	obs.send(t, protocol.CmdRestart, protocol.RestartPayload{Args: []string{}, Env: []string{}})
	third := receiveSourceLaunch(t, launches)
	if third.Program != artifact.program || third.Cwd != dir || third.Args == nil || len(third.Args) != 0 ||
		third.Env == nil || len(third.Env) != 0 || builds.Load() != 1 {
		t.Fatalf("restart overrides rebuilt or lost cwd/nil semantics: %+v, builds=%d", third, builds.Load())
	}
	_ = hh.client.Close()
	deadline := time.Now().Add(3 * time.Second)
	for hb.ClientCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if hb.ClientCount() != 1 {
		t.Fatal("driver disconnect did not leave only the observer")
	}
	requireArtifact(t, artifact, true)
	_ = obs.Close()
	awaitSourceSignal(t, cleanupSeen)
	select {
	case <-hb.Done():
		t.Fatal("session completed while debugger cleanup was retained")
	default:
	}
	requireArtifact(t, artifact, true)
	cleanupOnce.Do(func() { close(cleanupGate) })
	awaitSourceSignal(t, hb.Done())
	hh.handler.artifacts.wg.Wait()
	requireArtifact(t, artifact, false)
}

func receiveSourceLaunch(t *testing.T, launches <-chan protocol.LaunchPayload) protocol.LaunchPayload {
	t.Helper()
	select {
	case launch := <-launches:
		return launch
	case <-time.After(5 * time.Second):
		t.Fatal("source launch did not reach the debugger")
		return protocol.LaunchPayload{}
	}
}
