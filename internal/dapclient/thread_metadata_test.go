package dapclient

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	godap "github.com/google/go-dap"
)

func TestDecodeThreadMetadataPreservesOpaqueHandles(t *testing.T) {
	tests := []struct {
		name    string
		threads string
		want    map[int]int64
	}{
		{
			name: "real g1 and synthetic current do not alias",
			threads: `[
				{"id":901,"name":"g1","bingoGoroutineId":1},
				{"id":1,"name":"stopped goroutine (unknown)"},
				{"id":407,"name":"g52","bingoGoroutineId":52}
			]`,
			want: map[int]int64{901: 1, 407: 52},
		},
		{
			name:    "legacy IDs and names are not identity proof",
			threads: `[{"id":1,"name":"g1"},{"id":52,"name":"g52"}]`,
		},
		{
			name:    "synthetic current does not claim a real goroutine",
			threads: `[{"id":901,"name":"stopped goroutine (unknown)"}]`,
		},
		{
			name:    "metadata is authoritative rather than the name",
			threads: `[{"id":407,"name":"g1","bingoGoroutineId":52}]`,
			want:    map[int]int64{407: 52},
		},
		{
			name:    "safe integer boundary is inclusive",
			threads: `[{"id":9007199254740991,"name":"g","bingoGoroutineId":9007199254740991}]`,
			want:    map[int]int64{9007199254740991: 9007199254740991},
		},
		{
			name:    "empty response",
			threads: `[]`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := threadMetadataResponse(test.threads)
			message, identities, err := DecodeProtocolMessageWithThreadMetadata(content)
			if err != nil {
				t.Fatal(err)
			}
			response, ok := message.(*godap.ThreadsResponse)
			if !ok {
				t.Fatalf("message type = %T, want *godap.ThreadsResponse", message)
			}
			if response.Seq != 7 || response.RequestSeq != 3 || !response.Success {
				t.Fatalf("response envelope changed: %+v", response.Response)
			}
			if !reflect.DeepEqual(identities, test.want) {
				t.Fatalf("identities = %v, want %v", identities, test.want)
			}
			legacy, err := DecodeProtocolMessage(content)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(message, legacy) {
				t.Fatalf("ordinary message changed: got %+v, want %+v", message, legacy)
			}
		})
	}
}

func TestDecodeThreadMetadataRejectsInvalidIdentityProof(t *testing.T) {
	tests := []struct {
		name    string
		threads string
	}{
		{"zero goid", `[{"id":901,"name":"g","bingoGoroutineId":0}]`},
		{"negative goid", `[{"id":901,"name":"g","bingoGoroutineId":-1}]`},
		{"null goid", `[{"id":901,"name":"g","bingoGoroutineId":null}]`},
		{"fractional goid", `[{"id":901,"name":"g","bingoGoroutineId":1.5}]`},
		{"string goid", `[{"id":901,"name":"g","bingoGoroutineId":"1"}]`},
		{"boolean goid", `[{"id":901,"name":"g","bingoGoroutineId":true}]`},
		{"object goid", `[{"id":901,"name":"g","bingoGoroutineId":{}}]`},
		{"array goid", `[{"id":901,"name":"g","bingoGoroutineId":[]}]`},
		{"unsafe goid", `[{"id":901,"name":"g","bingoGoroutineId":9007199254740992}]`},
		{"overflowing goid", `[{"id":901,"name":"g","bingoGoroutineId":9223372036854775808}]`},
		{"zero handle", `[{"id":0,"name":"g","bingoGoroutineId":1}]`},
		{"negative handle", `[{"id":-1,"name":"g","bingoGoroutineId":1}]`},
		{"unsafe handle", `[{"id":9007199254740992,"name":"g","bingoGoroutineId":1}]`},
		{"duplicate real handles", `[
			{"id":901,"name":"g1","bingoGoroutineId":1},
			{"id":901,"name":"g2","bingoGoroutineId":2}
		]`},
		{"synthetic handle aliases a real entry", `[
			{"id":901,"name":"g1","bingoGoroutineId":1},
			{"id":901,"name":"stopped goroutine (unknown)"}
		]`},
		{"invalid unannotated sibling handle", `[
			{"id":901,"name":"g1","bingoGoroutineId":1},
			{"id":0,"name":"stopped goroutine (unknown)"}
		]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, identities, err := DecodeProtocolMessageWithThreadMetadata(threadMetadataResponse(test.threads))
			if err == nil {
				t.Fatal("invalid identity proof accepted")
			}
			if message != nil || identities != nil {
				t.Fatalf("failed decode returned partial success: message=%T identities=%v", message, identities)
			}
		})
	}
}

func TestThreadMetadataReaderPreservesFramingAndNormalMessages(t *testing.T) {
	contents := []string{
		string(threadMetadataResponse(`[{"id":901,"name":"g1","bingoGoroutineId":1}]`)),
		`{"seq":8,"type":"event","event":"initialized"}`,
		`{"seq":9,"type":"event","event":"bingo/session/v1","body":{"version":1,"sessionId":"session-123"}}`,
		`{"seq":10,"type":"response","request_seq":4,"success":false,"command":"threads","message":"not stopped"}`,
		`{"seq":11,"type":"request","command":"threads","arguments":{}}`,
	}
	var wire bytes.Buffer
	for _, content := range contents {
		if _, err := fmt.Fprintf(&wire, "Content-Length: %d\r\n\r\n%s", len(content), content); err != nil {
			t.Fatal(err)
		}
	}
	reader := bufio.NewReader(&wire)
	for i, content := range contents {
		message, identities, err := ReadProtocolMessageWithThreadMetadata(reader)
		if err != nil {
			t.Fatalf("read message %d: %v", i, err)
		}
		normal, err := DecodeProtocolMessage([]byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(message, normal) {
			t.Fatalf("message %d changed: got %T, want %T", i, message, normal)
		}
		if i == 0 {
			if !reflect.DeepEqual(identities, map[int]int64{901: 1}) {
				t.Fatalf("identities = %v", identities)
			}
		} else if identities != nil {
			t.Fatalf("message %d has unexpected thread metadata: %v", i, identities)
		}
	}
	if _, _, err := ReadProtocolMessageWithThreadMetadata(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream error = %v, want EOF", err)
	}
}

func TestThreadMetadataReaderReportsTransportAndDecodeErrors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		reader := bufio.NewReader(bytes.NewBufferString("Content-Length: 200\r\n\r\n{}"))
		message, identities, err := ReadProtocolMessageWithThreadMetadata(reader)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v, want unexpected EOF", err)
		}
		if message != nil || identities != nil {
			t.Fatal("transport failure returned partial success")
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		message, identities, err := DecodeProtocolMessageWithThreadMetadata([]byte(`{"seq":`))
		if err == nil || message != nil || identities != nil {
			t.Fatalf("decode = (%T, %v, %v)", message, identities, err)
		}
	})
}

func threadMetadataResponse(threads string) []byte {
	return []byte(fmt.Sprintf(`{"seq":7,"type":"response","request_seq":3,"success":true,"command":"threads","body":{"threads":%s}}`, threads))
}
