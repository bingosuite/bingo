package ws

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Protocol", func() {
	Describe("Message", func() {
		It("should handle Message struct", func() {
			data, _ := json.Marshal(map[string]string{"key": "value"})
			msg := Message{
				Type: string(EventStateUpdate),
				Data: data,
			}

			Expect(msg.Type).To(Equal(string(EventStateUpdate)))
			Expect(msg.Data).NotTo(BeNil())

			jsonData, err := json.Marshal(msg)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaledMsg Message
			err = json.Unmarshal(jsonData, &unmarshaledMsg)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaledMsg.Type).To(Equal(msg.Type))
		})
	})

	Describe("ContinueCmd", func() {
		It("should handle ContinueCmd struct", func() {
			cmd := ContinueCmd{
				Type:      CmdContinue,
				SessionID: "session-1",
			}

			Expect(cmd.Type).To(Equal(CmdContinue))
			Expect(cmd.SessionID).To(Equal("session-1"))

			data, err := json.Marshal(cmd)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled ContinueCmd
			err = json.Unmarshal(data, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaled.SessionID).To(Equal(cmd.SessionID))
		})
	})
})
