package ws_client

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"nhooyr.io/websocket"
	"sync"
	"time"
)

const (
	ConnectEvent    = "$connect"
	DisConnectEvent = "$disconnect"
)

func initConfig(cfg *ClientConfig) {
	if cfg == nil {
		cfg = getBaseConfig()
		return
	}

	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = time.Second * 10
	}

}

func getBaseConfig() *ClientConfig {
	return &ClientConfig{
		AutoReconnect: true,
		WriteTimeout:  time.Second * 5,
		//EmitsRepeatOnError: false,
	}
}

type socketIO interface {
	ReadSocketIOMessage(bytes []byte) (interface{}, error)
	MakeEventMsg(namespace, event string, data []byte) []byte
	MakeConnectMsg(namespace string) []byte
}

type ClientConfig struct {
	Url           string
	AutoReconnect bool
	WriteTimeout  time.Duration
	//EmitsRepeatOnError bool
}

type handlerName struct {
	namespace, event string
}

type handler func(msg []byte)

type WriteEvent struct {
	namespace, event string
	data             []byte
}

type Client struct {
	handlers         map[handlerName]handler
	socketIO         socketIO
	conn             *websocket.Conn
	config           *ClientConfig
	connDownCh       chan struct{}
	pingCh           chan struct{}
	writeCh          chan *WriteEvent
	mu               sync.RWMutex
	customNamespaces []string
}

func NewClient(cfg *ClientConfig) *Client {
	initConfig(cfg)
	client := Client{
		handlers:   make(map[handlerName]handler, 100),
		socketIO:   NewSocketIO(NewEngineIO()),
		config:     cfg,
		pingCh:     make(chan struct{}),
		connDownCh: make(chan struct{}),
		writeCh:    make(chan *WriteEvent, 1000),
	}
	return &client
}

func (c *Client) Connect() *Client {
	var err error

	c.conn, _, err = websocket.Dial(context.TODO(), c.config.Url, nil)

	if err != nil {
		if !c.config.AutoReconnect {
			log.Fatal(err)
		} else {
			log.Println(err)
			go signalConnDown(c.connDownCh)
		}
	}

	go func() {
		go c.reconnect()

		for {
			if c.conn != nil {
				break
			}
			time.Sleep(time.Millisecond * 500)
		}

		for _, v := range c.customNamespaces {
			c.connectNamespace(v)
		}

		go c.read()
		go c.eventWriter()
	}()

	return c
}

func (c *Client) read() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for {
		func() {
			c.mu.RLock()
			defer c.mu.RUnlock()

			_, data, err := c.conn.Read(ctx)
			if err != nil {
				log.Println(err)
				go signalConnDown(c.connDownCh)
				return
			}

			log.Printf("ROW READ DATA: %s", string(data))

			go func(d []byte) {
				v, err := c.socketIO.ReadSocketIOMessage(d)
				if err != nil {
					log.Println(err)
				}

				switch s := v.(type) {
				case *Event:
					c.runEvent(s.namespace, s.event, s.data)
				case *OpenEvent:
					go c.ping(s, c.pingCh)
					c.runEvent("/", ConnectEvent, s.data)
				}

				if err != nil {
					log.Println(err)
				}

			}(data)

		}()
	}
}

func (c *Client) eventWriter() {
	for e := range c.writeCh {
		func() {
			c.mu.RLock()
			defer c.mu.RUnlock()

			ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
			defer cancel()

			if err := c.conn.Write(ctx, websocket.MessageText, c.socketIO.MakeEventMsg(e.namespace, e.event, e.data)); err != nil {
				log.Println(err)

				go signalConnDown(c.connDownCh)

				//if c.config.EmitsRepeatOnError { // не работает
				//	go func(e *WriteEvent) {
				//		c.writeCh <- e
				//	}(e)
				//}

			}
		}()
	}
}

func (c *Client) ping(e *OpenEvent, closeCh chan struct{}) {
	var h HandshakeAnswer
	if err := json.Unmarshal(e.data, &h); err != nil {
		log.Fatal(err)
	}

	pingInterval := time.Millisecond * time.Duration(h.PingInterval)

	for {
		err := func() error {
			c.mu.RLock()
			defer c.mu.RUnlock()

			select {
			case <-closeCh:
				close(closeCh)
				return errors.New("this ping need be closed")
			default:
				ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
				defer cancel()

				if err := c.conn.Write(ctx, websocket.MessageText, []byte("22")); err != nil {
					log.Println(err)

					go signalConnDown(c.connDownCh)
				}
			}

			return nil
		}()

		if err != nil {
			log.Println("CLOSE PING", err)
			break
		}

		time.Sleep(pingInterval)
	}
}

func signalConnDown(downCh chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("down channel closed")
		}
	}()

	downCh <- struct{}{}
}

func (c *Client) close(status websocket.StatusCode, reason string) {
	err := c.conn.Close(status, reason)
	if err != nil {
		log.Println(err)
	}
}

func (c *Client) reconnect() {
	var err error
	for {
		<-c.connDownCh
		c.mu.Lock()
		close(c.connDownCh)
		c.connDownCh = make(chan struct{})

		go func(pingCh chan struct{}) {
			pingCh <- struct{}{}
		}(c.pingCh)

		go c.runEvent("/", DisConnectEvent, nil)

		c.pingCh = make(chan struct{})

		func() {
			defer c.mu.Unlock()
			for {
				c.conn, _, err = websocket.Dial(context.Background(), c.config.Url, nil)
				if err != nil {
					log.Println(err)
					time.Sleep(time.Second * 5)
				} else {
					break
				}
			}
		}()

		for _, v := range c.customNamespaces {
			c.connectNamespace(v)
		}

	}
}

func (c *Client) Emit(namespace, event string, data []byte) {
	go func() {
		c.writeCh <- &WriteEvent{namespace, event, data}
	}()
}

func (c *Client) connectNamespace(namespace string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageText, c.socketIO.MakeConnectMsg(namespace)); err != nil {
		log.Println(err)

		go signalConnDown(c.connDownCh)
	}
}

func (c *Client) ConnectToCustomNamespace(namespace string) {
	c.customNamespaces = append(c.customNamespaces, namespace)
}

func (c *Client) OnEvent(namespace, event string, f handler) {
	c.handlers[handlerName{namespace, event}] = f
}

func (c *Client) OnConnect(f handler) {
	c.handlers[handlerName{"/", ConnectEvent}] = f
}

func (c *Client) OnDisconnect(f handler) {
	c.handlers[handlerName{"/", DisConnectEvent}] = f
}

func (c *Client) runEvent(namespace, event string, data []byte) {
	if namespace == "" {
		namespace = "/"
	}

	if _, ok := c.handlers[handlerName{namespace, event}]; ok {
		c.handlers[handlerName{namespace, event}](data)
	} else {
		log.Printf("not exist event `%s` in namespace `%s`", event, namespace)
	}

}

//func (c *Client) OnError(namespace string, data interface{}) {
//
//}
