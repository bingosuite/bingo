package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestLegacyLaunchOmitsWorkingDirectory(t *testing.T) {
	data, err := json.Marshal(protocol.LaunchPayload{Program: "/tmp/app"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"program":"/tmp/app"}` {
		t.Fatalf("legacy LaunchPayload wire changed: %s", data)
	}
	var legacy protocol.LaunchPayload
	if err := json.Unmarshal([]byte(`{"program":"/tmp/app","args":["old-client"]}`), &legacy); err != nil || legacy.Cwd != "" {
		t.Fatalf("legacy launch must inherit server cwd: %+v, %v", legacy, err)
	}
}
