package debugger

import (
	"debug/gosym"
	"testing"

	"github.com/bingosuite/bingo/internal/debuginfo"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDebugger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Debugger Suite")
}

type testDebugInfo struct {
	target      debuginfo.Target
	lineToPCErr error
}

func (t *testDebugInfo) GetTarget() debuginfo.Target {
	return t.target
}

func (t *testDebugInfo) PCToLine(pc uint64) (string, int, *gosym.Func) {
	return "", 0, nil
}

func (t *testDebugInfo) LineToPC(file string, line int) (uint64, *gosym.Func, error) {
	if t.lineToPCErr != nil {
		return 0, nil, t.lineToPCErr
	}
	return 0, nil, nil
}

func (t *testDebugInfo) LookupFunc(fn string) *gosym.Func {
	return nil
}
