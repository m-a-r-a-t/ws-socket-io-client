package ws_client

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type engineIO interface {
	ReadPacketType(b byte) (EngineIOPacketType, error)
	AddPacketType(msg *Msg, packet EngineIOPacketType)
}

type SocketIOPacketType string

const (
	ConnectPacket      SocketIOPacketType = "0"
	DisconnectPacket   SocketIOPacketType = "1"
	EventPacket        SocketIOPacketType = "2"
	AckPacket          SocketIOPacketType = "3"
	ConnectErrorPacket SocketIOPacketType = "4"
	BinaryEventPacket  SocketIOPacketType = "5"
	BinaryAckPacket    SocketIOPacketType = "6"
)

type SocketIO struct {
	engineIO engineIO
}

func NewSocketIO(engineIO engineIO) *SocketIO {
	return &SocketIO{engineIO: engineIO}
}

func (s *SocketIO) addPacketType(msg *Msg, packet SocketIOPacketType) {
	msg.Add([]byte(packet))
}

func (s *SocketIO) readPacketType(b byte) (SocketIOPacketType, error) {
	switch string(b) {
	case "0":
		return ConnectPacket, nil
	case "1":
		return DisconnectPacket, nil
	case "2":
		return EventPacket, nil
	case "3":
		return AckPacket, nil
	case "4":
		return ConnectErrorPacket, nil
	case "5":
		return BinaryEventPacket, nil
	case "6":
		return BinaryAckPacket, nil
	}

	return "", errors.New("not exist socket io packet type")
}

type Msg struct {
	buf []byte
}

func (m *Msg) Add(bytes []byte) {
	m.buf = append(m.buf, bytes...)
}

func newMsg(bufSize int) *Msg {
	return &Msg{
		buf: make([]byte, 0, bufSize),
	}
}

func (s *SocketIO) MakeEventMsg(namespace, event string, data []byte, ackId *int) []byte {
	msg := newMsg(len(data) + 100)

	s.engineIO.AddPacketType(msg, MessagePacket)
	s.addPacketType(msg, EventPacket)

	if namespace != "/" {
		msg.Add([]byte(fmt.Sprintf("%s,", namespace)))
	}
	// 4215[hello,{name: 'world'}] 15 is id

	if ackId != nil {
		msg.Add([]byte(strconv.Itoa(*ackId)))
	}

	msg.Add([]byte("["))

	msg.Add([]byte(fmt.Sprintf(`"%s",`, event)))

	msg.Add(data)

	//bytes, err := json.Marshal(data) // if data is `interface{}` type
	//if err != nil {
	//	panic(err)
	//}
	//
	//msg.Add(bytes)

	msg.Add([]byte("]"))

	return msg.buf
}

func (s *SocketIO) MakeConnectMsg(namespace string) []byte {
	msg := newMsg(len(namespace) + 10)

	s.engineIO.AddPacketType(msg, MessagePacket)
	s.addPacketType(msg, ConnectPacket)

	if namespace != "/" {
		msg.Add([]byte(fmt.Sprintf("%s", namespace)))
	}

	return msg.buf
}

func (s *SocketIO) ReadSocketIOMessage(bytes []byte) (interface{}, error) {
	engineIOPacket, err := s.engineIO.ReadPacketType(bytes[0])
	if err != nil {
		return nil, err
	}

	socketIOPacket, err := s.readPacketType(bytes[1])
	if err != nil && engineIOPacket != OpenPacket {
		return nil, err
	}

	switch engineIOPacket {
	case OpenPacket:
		return &OpenEvent{data: bytes[1:]}, nil
	case PongPacket:
		// skip
		return nil, nil
	case MessagePacket:
		switch socketIOPacket {
		case ConnectPacket:
			//skip
			return nil, nil
		case EventPacket:
			return s.readMessageEvent(bytes[2:])
		case AckPacket:
			return s.readMessageAck(bytes[2:])
		default:
			return nil, fmt.Errorf("invalid socketIO Packet: %s", socketIOPacket)
		}
	default:
		return nil, fmt.Errorf("invalid engineIO Packet: %s", engineIOPacket)
	}
}

type Event struct {
	namespace, event string
	data             []byte
}

type OpenEvent struct {
	data []byte
}

type Ack struct {
	Id        int
	Namespace string
	Data      []byte
}

func (s *SocketIO) readMessageAck(bytes []byte) (*Ack, error) {
	namespace, l, err := s.getNamespace(bytes)
	if err != nil {
		return nil, err
	}

	ackId, l, err := s.getAckId(bytes)
	if err != nil {
		return nil, err
	}

	data, err := s.getData(bytes[l:])
	if err != nil {
		return nil, err
	}

	return &Ack{
		ackId,
		namespace,
		data,
	}, nil
}

func (s *SocketIO) readMessageEvent(bytes []byte) (*Event, error) {
	namespace, l, err := s.getNamespace(bytes)
	if err != nil {
		return nil, err
	}

	event, l, err := s.getEvent(bytes[l:])
	if err != nil {
		return nil, err
	}

	data, err := s.getData(bytes[l:])
	if err != nil {
		return nil, err
	}

	return &Event{
		namespace,
		event,
		data,
	}, nil
}

func (s *SocketIO) getNamespace(bytes []byte) (string, int, error) {
	var (
		b         strings.Builder
		isCorrect bool
	)

	if len(bytes) != 0 && bytes[0] != '/' {
		return "", 0, nil
	}

	for i := 0; i < len(bytes); i++ {
		if bytes[i] == ',' && b.Len() != 0 {
			isCorrect = true
			break
		}
		b.WriteByte(bytes[i])
	}

	if !isCorrect {
		return "", 0, errors.New("cannot parse namespace")
	}
	// + 1 так как еще ','
	return b.String(), len([]rune(b.String())) + 1, nil
}

func (s *SocketIO) getAckId(bytes []byte) (id int, n int, err error) {
	var b strings.Builder

	if len(bytes) == 0 {
		return 0, 0, errors.New("cannot parse data")
	}

	for i := range bytes {
		if bytes[i] == '[' {
			break
		}

		b.WriteByte(bytes[i])
	}

	v, err := strconv.ParseInt(b.String(), 10, 64)
	if err != nil {
		return 0, 0, errors.New("can not parse id from string")
	}

	// +4 так как учитываем ["",
	return int(v), len([]rune(b.String())) + 1, nil
}

func (s *SocketIO) getEvent(bytes []byte) (string, int, error) {
	var (
		b         strings.Builder
		isCorrect bool
	)

	if len(bytes) != 0 && bytes[0] != '[' {
		return "", 0, errors.New("cannot parse event")
	}

	for i := 2; i < len(bytes); i++ {
		if bytes[i] == '"' && b.Len() != 0 {
			isCorrect = true
			break
		}
		b.WriteByte(bytes[i])
	}

	if !isCorrect {
		return "", 0, errors.New("cannot parse event")
	}
	// +4 так как учитываем ["",
	return b.String(), len([]rune(b.String())) + 4, nil
}

func (s *SocketIO) getData(bytes []byte) ([]byte, error) {
	if len(bytes) == 0 {
		return nil, errors.New("cannot parse data")
	}

	switch bytes[0] {
	case '"', '[', '{':
		if getBracketPair(bytes[0]) != bytes[len(bytes)-3] {
			return nil, errors.New("cannot parse data")
		}
	}

	return bytes[:len(bytes)-2], nil
}

func getBracketPair(b byte) byte {
	switch b {
	case '"':
		return '"'
	case '[':
		return ']'
	case '{':
		return '}'
	}

	return '-'
}
