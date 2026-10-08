//go:build e2e

package integration

import (
	"time"

	godap "github.com/google/go-dap"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const channelContentsTargetSrc = `package main
import (
	"fmt"
	"os"
)
type item struct { Name string; Value int; Pointer *int }
type queue chan item
func main() {
	number := 42
	values := make(queue, 3)
	values <- item{Name:"discard"}
	values <- item{Name:"first", Value:10, Pointer:&number}
	values <- item{Name:"second", Value:20}
	<-values
	values <- item{Name:"third", Value:30}
	close(values)
	receive := (<-chan item)(values)
	send := (chan<- item)(values)
	unbuffered := make(chan int)
	go func() { unbuffered <- 99 }()
	var nilChannel chan int
	zeroSize := make(chan struct{}, 2)
	zeroSize <- struct{}{}
	zeroSize <- struct{}{}
	integers := make(chan int, 1)
	integers <- 42
	nested := make(chan chan int, 1)
	nested <- integers
	fmt.Println(values, receive, send, unbuffered, nilChannel, zeroSize, nested) // CHANNEL_BP
	for i, name := range []string{"first", "second", "third"} {
		value, ok := <-values
		if !ok || value.Name != name || value.Value != (i+1)*10 { os.Exit(61) }
		if i == 0 && (value.Pointer == nil || *value.Pointer != 42) { os.Exit(62) }
	}
	if _, ok := <-values; ok { os.Exit(63) }
	if <-unbuffered != 99 || len(zeroSize) != 2 || len(nested) != 1 || len(integers) != 1 { os.Exit(64) }
	os.Exit(43)
}
`

func declareDAPChannelContentsSpec() {
	It("inspects typed channel contents through DAP Variables and Watch without consuming them", Label("dap", "inspect", "channels"), func() {
		bin := buildTarget("channel_contents_target", channelContentsTargetSrc)
		_, _, dapAddr := startTestServerWithDAP()
		dc := dialDAP(dapAddr)
		dc.initialize()
		launchSeq := dc.launch(bin, false)
		dc.waitEvent(20*time.Second, "initialized")
		bps := dc.setBreakpoints("channel_contents_target.go", markerLine(channelContentsTargetSrc, "// CHANNEL_BP"))
		Expect(bps.Body.Breakpoints).To(HaveLen(1))
		Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue())
		dc.configurationDone()
		Expect(dc.await(launchSeq).(godap.ResponseMessage).GetResponse().Success).To(BeTrue())
		stopped := dc.waitStopped(20 * time.Second)
		frames := dc.stackTrace(stopped.Body.ThreadId)
		Expect(frames.Body.StackFrames).NotTo(BeEmpty())
		frameID := frames.Body.StackFrames[0].Id
		scopes := dc.scopes(frameID)
		Expect(scopes.Body.Scopes).To(HaveLen(1))
		locals := channelDAPVariables(dc, scopes.Body.Scopes[0].VariablesReference)
		for _, name := range []string{"values", "receive", "send"} {
			channel := locals[name]
			Expect(channel.Value).To(ContainSubstring("len:3 cap:3 closed:true"))
			assertChannelDAPContents(dc, channel.VariablesReference)
			watch := dc.evaluate(name, frameID, "watch")
			Expect(watch.GetResponse().Success).To(BeTrue())
			Expect(watch.Body.Result).To(ContainSubstring("len:3 cap:3 closed:true"))
			assertChannelDAPContents(dc, watch.Body.VariablesReference)
		}
		Expect(locals["nilChannel"].Value).To(ContainSubstring("(nil)"))
		Expect(locals["nilChannel"].VariablesReference).To(Equal(0))
		Expect(locals["unbuffered"].Value).To(ContainSubstring("unbuffered; no stored values"))
		Expect(channelDAPVariables(dc, locals["unbuffered"].VariablesReference)).To(HaveLen(3))
		empty := channelDAPVariables(dc, locals["zeroSize"].VariablesReference)
		Expect(empty).To(HaveKey("[0]"))
		Expect(empty).To(HaveKey("[1]"))
		nested := channelDAPVariables(dc, locals["nested"].VariablesReference)
		integers := channelDAPVariables(dc, nested["[0]"].VariablesReference)
		Expect(integers["[0]"].Value).To(Equal("42"))

		dc.continueThread(stopped.Body.ThreadId)
		exited := dc.waitEvent(20*time.Second, "exited").(*godap.ExitedEvent)
		Expect(exited.Body.ExitCode).To(Equal(43), "the target receives every original buffered value in order")
		dc.waitEvent(5*time.Second, "terminated")
		dc.disconnect()
	})
}

func channelDAPVariables(dc *dapClient, reference int) map[string]godap.Variable {
	GinkgoHelper()
	Expect(reference).To(BeNumerically(">", 0))
	response := dc.variablesResp(reference)
	Expect(response.GetResponse().Success).To(BeTrue())
	variables := make(map[string]godap.Variable)
	for _, v := range response.Body.Variables {
		variables[v.Name] = v
	}
	return variables
}

func assertChannelDAPContents(dc *dapClient, reference int) {
	GinkgoHelper()
	values := channelDAPVariables(dc, reference)
	Expect(values["len"].Value).To(Equal("3"))
	Expect(values["cap"].Value).To(Equal("3"))
	Expect(values["closed"].Value).To(Equal("true"))
	for i, name := range []string{"first", "second", "third"} {
		childName := []string{"[0]", "[1]", "[2]"}[i]
		fields := channelDAPVariables(dc, values[childName].VariablesReference)
		Expect(fields["Name"].Value).To(Equal(`"` + name + `"`))
		Expect(fields["Value"].Value).To(Equal([]string{"10", "20", "30"}[i]))
		if i == 0 {
			pointer := channelDAPVariables(dc, fields["Pointer"].VariablesReference)
			Expect(pointer["*Pointer"].Value).To(Equal("42"))
		}
	}
}
