//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	godap "github.com/google/go-dap"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/dapclient"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const sourceLaunchTarget = `package main

import (
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	cwd, err := os.Getwd()
	if err != nil { panic(err) }
	program, err := os.Executable()
	if err != nil { panic(err) }
	data := []byte{}
	if os.Getenv("BINGO_READ_DATA") == "1" {
		data, err = os.ReadFile("data.txt")
		if err != nil { panic(err) }
	}
	report, err := json.Marshal(map[string]string{
		"cwd": cwd, "pwd": os.Getenv("PWD"), "program": program,
		"data": string(data), "arg": os.Args[2],
	})
	if err != nil { panic(err) }
	if err := os.WriteFile(os.Args[1], report, 0600); err != nil { panic(err) }
	fmt.Println("source launch reached main") // SOURCE_BP
}
`

func declareDAPSourceLaunchSpec() {
	for _, explicitCwd := range []bool{false, true} {
		name := "defaults to the source package directory"
		if explicitCwd {
			name = "resolves a source package against an explicit working directory"
		}
		It(name, Label("dap", "source-launch"), func() {
			runSourceLaunchSpec(explicitCwd)
		})
	}
	It("honors executable cwd and preserves the legacy empty-cwd default", Label("dap", "source-launch"), func() {
		runExecutableCwdSpec()
	})
}

func runSourceLaunchSpec(explicitCwd bool) {
	root := GinkgoT().TempDir()
	pkgDir := filepath.Join(root, "source package with spaces")
	Expect(os.Mkdir(pkgDir, 0o700)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(pkgDir, "go.mod"), []byte("module example.test/source\n\ngo 1.25.0\n"), 0o600)).To(Succeed())
	Expect(os.WriteFile(filepath.Join(pkgDir, "main.go"), []byte(sourceLaunchTarget), 0o600)).To(Succeed())
	cwd, program := pkgDir, pkgDir
	arguments := map[string]any{"mode": "debug", "program": program}
	if explicitCwd {
		cwd = filepath.Join(root, "working directory with spaces")
		Expect(os.Mkdir(cwd, 0o700)).To(Succeed())
		program = filepath.Join("..", filepath.Base(pkgDir))
		arguments["program"], arguments["cwd"] = program, cwd
	}
	Expect(os.WriteFile(filepath.Join(cwd, "data.txt"), []byte("relative package data"), 0o600)).To(Succeed())
	reportPath := filepath.Join(root, "report.json")
	arguments["args"] = []string{reportPath, "argument with spaces"}
	arguments["env"] = []string{"GOWORK=off", "GOTOOLCHAIN=local", "BINGO_READ_DATA=1", "PWD=/stale/tracee/directory"}
	wire, err := json.Marshal(arguments)
	Expect(err).NotTo(HaveOccurred())

	_, wsAddr, dapAddr := startTestServerWithDAP()
	dc := dialDAP(dapAddr)
	dc.initialize()
	launchSeq := dc.send("launch", &godap.LaunchRequest{Arguments: wire})
	ready := dc.waitEvent(130*time.Second, protocol.DAPSessionEventName, "terminated")
	if ready.GetEvent().Event == "terminated" {
		Fail("source launch failed: " + dc.await(launchSeq).(godap.ResponseMessage).GetResponse().Message)
	}
	sessionID := ready.(*dapclient.SessionEvent).Body.SessionID
	observer, err := client.Join(wsAddr, sessionID)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() { _ = observer.Close() })
	Expect(observer.RequestGoroutineSnapshot()).To(Succeed())
	snapshot := awaitEvent(observer.Events(), 15*time.Second, protocol.EventGoroutineSnapshot, protocol.EventError)
	Expect(snapshot.Kind).To(Equal(protocol.EventGoroutineSnapshot), "immediate observer query must see the installed debugger: %s", snapshot.Payload)
	dc.waitEvent(20*time.Second, "initialized")
	bps := dc.setBreakpoints(filepath.Join(pkgDir, "main.go"), markerLine(sourceLaunchTarget, "// SOURCE_BP"))
	Expect(bps.Body.Breakpoints).To(HaveLen(1))
	Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue())
	dc.configurationDone()
	Expect(dc.await(launchSeq).(godap.ResponseMessage).GetResponse().Success).To(BeTrue())
	Expect(dc.waitStopped(20 * time.Second).Body.Reason).To(Equal("breakpoint"))
	report := sourceLaunchReport(reportPath)
	Expect(report["cwd"]).To(Equal(cwd))
	Expect(report["pwd"]).To(Equal(cwd))
	Expect(report["data"]).To(Equal("relative package data"))
	Expect(report["arg"]).To(Equal("argument with spaces"))
	binary := report["program"]
	Expect(filepath.Base(binary)).To(Equal("debuggee"))
	Expect(filepath.Base(filepath.Dir(binary))).To(HavePrefix("bingo-dap-build-"))
	Expect(os.Remove(reportPath)).To(Succeed())
	restart := dc.request("restart", &godap.RestartRequest{})
	Expect(restart.(godap.ResponseMessage).GetResponse().Success).To(BeTrue())
	Expect(dc.waitStopped(20 * time.Second).Body.Reason).To(Equal("breakpoint"))
	Expect(sourceLaunchReport(reportPath)).To(Equal(report), "restart must reuse the build, cwd, args and environment")

	dc.disconnect()
	Eventually(func() bool {
		sessions, err := client.ListSessions(wsAddr)
		return err == nil && len(sessions) == 1 && sessions[0].Clients == 1 && sessions[0].State == protocol.StateIdle
	}, 15*time.Second, 20*time.Millisecond).Should(BeTrue())
	_, err = os.Stat(binary)
	Expect(err).NotTo(HaveOccurred(), "observer keeps the session's restart artifact alive")
	Expect(observer.Close()).To(Succeed())
	Eventually(func() bool {
		_, err := os.Stat(filepath.Dir(binary))
		return os.IsNotExist(err)
	}, 15*time.Second, 20*time.Millisecond).Should(BeTrue(), "the exact build directory retires with the hub")
}

func sourceLaunchReport(path string) map[string]string {
	GinkgoHelper()
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	var report map[string]string
	Expect(json.Unmarshal(data, &report)).To(Succeed())
	return report
}

func runExecutableCwdSpec() {
	binary := buildTarget("cwd_target", sourceLaunchTarget)
	serverCwd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	for _, cwd := range []string{GinkgoT().TempDir(), ""} {
		_, _, dapAddr := startTestServerWithDAP()
		dc := dialDAP(dapAddr)
		dc.initialize()
		reportPath := filepath.Join(GinkgoT().TempDir(), "report.json")
		arguments := map[string]any{
			"program": binary,
			"args":    []string{reportPath, "legacy argument"},
			"env":     []string{"BINGO_READ_DATA=0"},
		}
		if cwd != "" {
			relative, err := filepath.Rel(cwd, binary)
			Expect(err).NotTo(HaveOccurred())
			arguments["program"], arguments["cwd"], arguments["mode"] = relative, cwd, "exec"
		}
		wire, err := json.Marshal(arguments)
		Expect(err).NotTo(HaveOccurred())
		launchSeq := dc.send("launch", &godap.LaunchRequest{Arguments: wire})
		dc.waitEvent(20*time.Second, "initialized")
		bps := dc.setBreakpoints("cwd_target.go", markerLine(sourceLaunchTarget, "// SOURCE_BP"))
		Expect(bps.Body.Breakpoints).To(HaveLen(1))
		Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue())
		dc.configurationDone()
		Expect(dc.await(launchSeq).(godap.ResponseMessage).GetResponse().Success).To(BeTrue())
		Expect(dc.waitStopped(20 * time.Second).Body.Reason).To(Equal("breakpoint"))
		report := sourceLaunchReport(reportPath)
		if cwd == "" {
			Expect(report["cwd"]).To(Equal(serverCwd))
			Expect(report["pwd"]).To(Equal(os.Getenv("PWD")))
		} else {
			Expect(report["cwd"]).To(Equal(cwd))
			Expect(report["pwd"]).To(Equal(cwd))
		}
		Expect(report["arg"]).To(Equal("legacy argument"))
		dc.disconnect()
	}
}
