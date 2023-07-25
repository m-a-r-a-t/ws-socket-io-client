package ws_client

import "errors"

type EngineIOPacketType string

type HandshakeAnswer struct {
	SID          string   `json:"sid"`
	Upgrades     []string `json:"upgrades"`
	PingInterval int      `json:"pingInterval"`
	PingTimeout  int      `json:"pingTimeout"`
	MaxPayload   int      `json:"maxPayload"`
}

const (
	OpenPacket    EngineIOPacketType = "0"
	ClosePacket   EngineIOPacketType = "1"
	PingPacket    EngineIOPacketType = "2"
	PongPacket    EngineIOPacketType = "3"
	MessagePacket EngineIOPacketType = "4"
	UpgradePacket EngineIOPacketType = "5"
	NoopPacket    EngineIOPacketType = "6"
)

type EngineIO struct {
}

func NewEngineIO() *EngineIO {
	return &EngineIO{}
}

func (e *EngineIO) ReadPacketType(b byte) (EngineIOPacketType, error) {
	switch string(b) {
	case "0":
		return OpenPacket, nil
	case "1":
		return ClosePacket, nil
	case "2":
		return PingPacket, nil
	case "3":
		return PongPacket, nil
	case "4":
		return MessagePacket, nil
	case "5":
		return UpgradePacket, nil
	case "6":
		return NoopPacket, nil
	}

	return "", errors.New("not exist engine io packet type")
}

func (e *EngineIO) AddPacketType(msg *Msg, packet EngineIOPacketType) {
	msg.Add([]byte(packet))
}
