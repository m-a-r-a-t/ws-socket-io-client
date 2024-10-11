package ws_client

import (
	"context"
	"encoding/json"
	"github.com/pkg/errors"
	"log"
	"log/slog"
	"math/rand"
	"nhooyr.io/websocket"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var ErrConnectionNotAvailable = errors.New("connection not available")

const (
	ConnectEvent    = "$connect"
	DisConnectEvent = "$disconnect"
)

const (
	defaultWriteTimeout      = 10 * time.Second
	defaultReconnectInterval = 2 * time.Second
)

func initDefault(cfg *ClientConfig) {
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
}

type socketIO interface {
	ReadSocketIOMessage(bytes []byte) (interface{}, error)
	MakeEventMsg(namespace, event string, data []byte, ackId *int) []byte
	MakeConnectMsg(namespace string) []byte
}

type ClientConfig struct {
	ConnectForce  bool
	Url           string
	AutoReconnect bool
	WriteTimeout  time.Duration
	//EmitsRepeatOnError bool
}

func (c *ClientConfig) Validate() error {
	if c.Url == "" {
		return errors.New("url is empty")
	}

	if c.WriteTimeout == 0 {
		return errors.New("write timeout is empty")
	}

	return nil
}

type handlerName struct {
	namespace, event string
}

type handlerFunc func(msg []byte)

type handler struct {
	isSync     bool
	f          handlerFunc
	eventQueue eventQueue
}

type eventQueue struct {
	ownerTicket    atomic.Int64
	nextFreeTicket atomic.Int64
}

type ackSubscriptions struct {
	mu   sync.RWMutex
	subs map[int]chan []byte
}

func (a *ackSubscriptions) Add(v chan []byte) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	for {
		newKey := rand.Int()
		if _, ok := a.subs[newKey]; !ok {
			a.subs[newKey] = v

			return newKey
		}
	}
}

func (a *ackSubscriptions) Get(k int) (chan []byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ch, ok := a.subs[k]

	return ch, ok
}

func (a *ackSubscriptions) Remove(k int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.subs[k]
	if !ok {
		return false
	}

	delete(a.subs, k)

	return true
}

func (a *ackSubscriptions) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()

	clear(a.subs)
}

type WriteEvent struct {
	ctx              context.Context
	writeResult      chan error
	ackCh            chan []byte
	namespace, event string
	data             []byte
}

type Client struct {
	started          atomic.Bool
	ackSubscriptions ackSubscriptions
	handlers         map[handlerName]*handler
	socketIO         socketIO
	conn             *websocket.Conn
	config           *ClientConfig
	startCh          chan struct{}
	stopCh           chan struct{}
	shutdownCh       chan struct{}
	shutDown         bool
	closed           bool
	writeCh          chan *WriteEvent
	mu               sync.RWMutex
	customNamespaces []string
}

func NewClient(cfg *ClientConfig) (*Client, error) {
	initDefault(cfg)

	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}

	initStartCh := make(chan struct{})
	close(initStartCh)

	c := &Client{
		ackSubscriptions: ackSubscriptions{
			subs: make(map[int]chan []byte, 1000),
		},
		handlers:   make(map[handlerName]*handler, 100),
		socketIO:   NewSocketIO(NewEngineIO()),
		config:     cfg,
		stopCh:     make(chan struct{}),
		startCh:    initStartCh,
		shutdownCh: make(chan struct{}),
		writeCh:    make(chan *WriteEvent, 1000),
	}

	return c, nil
}

func (c *Client) Connect() error {
	if c.started.Load() == true {
		return errors.New("client started, create new client")
	}

	conn, err := c.connect()
	if err != nil && !c.config.ConnectForce {
		_ = c.Close()

		return errors.Wrap(err, "can not connect to server")
	}

	c.conn = conn

	c.started.Swap(true)

	go func() {
		for range c.shutdownCh {
			c.shutdown()
		}
	}()

	//for _, v := range c.customNamespaces {
	//	c.connectNamespace(v)
	//}

	go func() {
		for {
			c.mu.RLock()
			startCh := c.startCh

			if c.closed {
				c.mu.RUnlock()
				return
			}

			c.mu.RUnlock()

			<-startCh

			err := c.read()
			if err != nil {
				slog.Error("Can not read from ws connection", slog.Any("err", err))
			}
		}
	}()

	go func() {
		for e := range c.writeCh {
			err := c.emit(e)
			if err != nil {
				slog.Debug("Can not emit message", slog.Any("err", err))
			}
		}
	}()

	return nil
}

func (c *Client) connect() (*websocket.Conn, error) {
	conn, _, err := websocket.Dial(context.TODO(), c.config.Url, nil)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	c.shutDown = true

	close(c.shutdownCh)

	if c.conn != nil {
		err := c.conn.Close(websocket.StatusNormalClosure, "normal closure")
		if err != nil {
			return errors.Wrap(err, "can not close ws connection")
		}
	}

	return nil
}

func (c *Client) IsClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.closed
}

func (c *Client) close(reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		err := c.conn.Close(websocket.StatusNormalClosure, reason)
		if err != nil {
			return errors.Wrap(err, "can not close ws connection")
		}
	}

	return nil
}

func (c *Client) read() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.signalShutDown()

		return errors.Wrap(err, "can not read from ws connection")
	}

	slog.Debug("MSG DATA", slog.String("data", string(data)))

	msg, err := c.socketIO.ReadSocketIOMessage(data)
	if err != nil {
		return errors.Wrap(err, "Can not parse SocketIO message")
	}

	switch s := msg.(type) {
	case *Ack:
		if ackCh, ok := c.ackSubscriptions.Get(s.Id); ok {
			ackCh <- s.Data
			c.ackSubscriptions.Remove(s.Id)
		}
	case *Event:
		namespace := s.namespace
		if namespace == "" {
			namespace = "/"
		}

		h, ok := c.handlers[handlerName{namespace, s.event}]
		if !ok {
			return errors.Errorf("not listen handler for event %s ; namespace %s", s.event, s.namespace)
		}

		ticket := h.eventQueue.nextFreeTicket.Add(1)

		go c.runHandler(namespace, s.event, s.data, ticket)
	case *OpenEvent:
		h, ok := c.handlers[handlerName{"/", ConnectEvent}]
		if !ok {
			return errors.Errorf("not listen handler for event %s", ConnectEvent)
		}
		// todo: где не нужен синхронный режим убрать тикеты.
		ticket := h.eventQueue.nextFreeTicket.Add(1)

		go c.startPinger(s)
		go c.runHandler("/", ConnectEvent, s.data, ticket)
	}

	return nil
}

func (c *Client) emit(e *WriteEvent) error {
	var ackId *int
	if e.ackCh != nil {
		v := c.ackSubscriptions.Add(e.ackCh)
		ackId = &v
	}

	// todo: надо чистить подписки после успешного получения и после отключения

	err := c.write(e.ctx, websocket.MessageText, c.socketIO.MakeEventMsg(e.namespace, e.event, e.data, ackId))
	if err != nil {
		e.writeResult <- err

		return errors.Wrap(err, "can not emit msg")
	}

	e.writeResult <- nil

	return nil
}

func (c *Client) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.conn == nil {
		c.signalShutDown()

		return ErrConnectionNotAvailable
	}

	if err := c.conn.Write(ctx, typ, data); err != nil {
		c.signalShutDown()

		return errors.Wrap(err, "can not write to ws connection")
	}

	return nil
}

func (c *Client) startPinger(e *OpenEvent) {
	var h HandshakeAnswer
	if err := json.Unmarshal(e.data, &h); err != nil {
		log.Fatal(err)
	}

	ticker := time.NewTicker(time.Millisecond * time.Duration(h.PingInterval))
	defer ticker.Stop()

	c.mu.RLock()
	if c.shutDown {
		c.mu.RUnlock()

		return
	}

	stopCh := c.stopCh
	c.mu.RUnlock()

	ping := func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
		defer cancel()

		err := c.write(ctx, websocket.MessageText, []byte("22"))
		if err != nil {
			slog.Error("can not ping server", slog.Any("err", err))
		}
	}

	for ; ; <-ticker.C {
		select {
		case <-stopCh:
			slog.Debug("CLOSE PING")
			return
		default:
			ping()
		}
	}
}

// call only in func with mutex
func (c *Client) signalShutDown() {
	if c.closed {
		return
	}

	select {
	case <-c.stopCh:
	case c.shutdownCh <- struct{}{}:
	}
}

func (c *Client) shutdown() {
	close(c.stopCh)

	_ = c.close("client shutdown")

	if !c.config.AutoReconnect {
		_ = c.Close()
		slog.Info("ws socket io client closed")

		return
	}

	c.mu.Lock()
	c.ackSubscriptions.clear()

	c.shutDown = true

	startCh := make(chan struct{})
	defer close(startCh)

	c.startCh = startCh // для остановки читающей горутины
	c.mu.Unlock()

	var conn *websocket.Conn
	var err error

	for {
		conn, err = c.connect()
		if err == nil || c.IsClosed() {
			break
		}

		slog.Info("Try connect to socket.io server again", slog.String("url", c.config.Url))
		time.Sleep(defaultReconnectInterval)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "client shutdown")
		}

		return
	}

	c.conn = conn
	c.shutDown = false
	c.stopCh = make(chan struct{})
}

func (c *Client) reconnect() {
	var conn *websocket.Conn
	var err error

	for {
		conn, err = c.connect()
		if err == nil || c.IsClosed() {
			break
		}

		slog.Info("Try connect to socket.io server again", slog.String("url", c.config.Url))
		time.Sleep(defaultReconnectInterval)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "client shutdown")
		}

		return
	}

	c.conn = conn

	c.stopCh = make(chan struct{})
}

func (c *Client) EmitWithAck(ctx context.Context, namespace, event string, data []byte) ([]byte, error) {
	c.mu.RLock()
	if c.closed || c.shutDown {
		c.mu.RUnlock()
		return nil, ErrConnectionNotAvailable
	}

	stopCh := c.stopCh
	c.mu.RUnlock()

	writeResultCh := make(chan error, 1)
	ackCh := make(chan []byte, 1)

	we := &WriteEvent{
		ctx,
		writeResultCh,
		ackCh,
		namespace,
		event,
		data,
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.writeCh <- we:
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-writeResultCh:
		if err != nil {
			return nil, err
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ackData := <-ackCh:
		return ackData, nil
	case <-stopCh:
		return nil, ErrConnectionNotAvailable
	}
}

func (c *Client) Emit(ctx context.Context, namespace, event string, data []byte) error {
	c.mu.RLock()
	if c.closed || c.shutDown {
		c.mu.RUnlock()
		return ErrConnectionNotAvailable
	}
	c.mu.RUnlock()

	writeResultCh := make(chan error, 1)
	defer close(writeResultCh)

	we := &WriteEvent{
		ctx,
		writeResultCh,
		nil,
		namespace,
		event,
		data,
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.writeCh <- we:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-writeResultCh:
		return err
	}
}

func (c *Client) connectNamespace(namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
	defer cancel()

	if err := c.write(ctx, websocket.MessageText, c.socketIO.MakeConnectMsg(namespace)); err != nil {
		return errors.Wrap(err, "can not connect to namespace")
	}

	return nil
}

func (c *Client) ConnectToCustomNamespace(namespace string) {
	c.customNamespaces = append(c.customNamespaces, namespace)
}

func (c *Client) OnEvent(namespace, event string, f handlerFunc) {
	c.handlers[handlerName{namespace, event}] = &handler{
		f: f,
	}
}

func (c *Client) OnEventSync(namespace, event string, f handlerFunc) {
	c.handlers[handlerName{namespace, event}] = &handler{
		isSync: true,
		f:      f,
	}
}

func (c *Client) OnConnect(f handlerFunc) {
	c.handlers[handlerName{"/", ConnectEvent}] = &handler{
		isSync: true,
		f:      f,
	}
}

func (c *Client) OnDisconnect(f handlerFunc) {
	c.handlers[handlerName{"/", DisConnectEvent}] = &handler{
		isSync: true,
		f:      f,
	}
}

//func (c *Client) OnError(namespace string, data interface{}) {
//
//}

func (c *Client) runHandler(namespace, event string, data []byte, ticket int64) {
	if namespace == "" {
		namespace = "/"
	}

	h, ok := c.handlers[handlerName{namespace, event}]
	if !ok {
		slog.Error(
			"not listen handler for event in namespace",
			slog.String("event", event),
			slog.String("namespace", namespace),
		)

		return
	}

	if h.isSync {
		for h.eventQueue.ownerTicket.Load() != ticket-1 {
			runtime.Gosched()
		}
	}

	h.f(data)

	h.eventQueue.ownerTicket.Add(1)
}
