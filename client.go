package ws_client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"nhooyr.io/websocket"
	"sync"
	"time"
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
		AutoReconnect:      true,
		WriteTimeout:       time.Second * 5,
		EmitsRepeatOnError: false,
	}
}

type socketIO interface {
	ReadSocketIOMessage(bytes []byte) (interface{}, error)
	MakeEventMsg(namespace, event string, data interface{}) []byte
	MakeConnectMsg(namespace string) []byte
}

type ClientConfig struct {
	Url                string
	AutoReconnect      bool
	WriteTimeout       time.Duration
	EmitsRepeatOnError bool
}

type handlerName struct {
	namespace, event string
}

type handler func(msg []byte)

type WriteEvent struct {
	namespace, event string
	data             interface{}
}

type Client struct {
	handlers         map[handlerName]handler
	socketIO         socketIO
	conn             *websocket.Conn
	config           *ClientConfig
	connDownCh       chan struct{}
	writeCh          chan *WriteEvent
	mu               sync.RWMutex
	customNamespaces []string
}

func Connect(cfg *ClientConfig) *Client {
	var err error
	initConfig(cfg)

	client := Client{
		handlers:   make(map[handlerName]handler, 100),
		socketIO:   NewSocketIO(NewEngineIO()),
		config:     cfg,
		connDownCh: make(chan struct{}),
		writeCh:    make(chan *WriteEvent, 1000),
	}

	client.conn, _, err = websocket.Dial(context.TODO(), cfg.Url, nil)
	go client.read() // TODO сделать провер
	go client.eventWriter()
	go client.reconnect()

	if err != nil {
		if !cfg.AutoReconnect {
			log.Fatal(err)
		} else {
			log.Println(err)
			go signalConnDown(client.connDownCh)
		}
	}

	return &client
}

func (c *Client) read() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for {
		func() {
			c.mu.RLock()
			defer c.mu.RUnlock()
			if c.conn == nil {
				return
			}

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
					go c.ping(s)
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
			if c.conn == nil {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
			defer cancel()

			if err := c.conn.Write(ctx, websocket.MessageText, c.socketIO.MakeEventMsg(e.namespace, e.event, e.data)); err != nil {
				c.mu.RUnlock()
				log.Println(err)

				signalConnDown(c.connDownCh)

				if c.config.EmitsRepeatOnError {
					fmt.Println("repeat")
					go func(e *WriteEvent) {
						c.writeCh <- e
					}(e)
				}

			}
		}()
	}
}

func (c *Client) ping(e *OpenEvent) {
	var h HandshakeAnswer
	if err := json.Unmarshal(e.data, &h); err != nil {
		signalConnDown(c.connDownCh)
	}

	pingInterval := time.Millisecond * time.Duration(h.PingInterval)

	for {
		err := func() error {
			var err error
			c.mu.RLock()
			defer c.mu.RUnlock()

			ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
			defer cancel()

			if err = c.conn.Write(ctx, websocket.MessageText, []byte("22")); err != nil {
				c.mu.RUnlock()
				log.Println(err)

				signalConnDown(c.connDownCh)
			}

			return err
		}()

		if err != nil {
			return
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

		for k, _ := range c.handlers {
			c.connectNamespace(k.namespace)
		}

	}
}

func (c *Client) Emit(namespace, event string, data interface{}) {
	c.writeCh <- &WriteEvent{namespace, event, data}
}

func (c *Client) connectNamespace(namespace string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.config.WriteTimeout)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageText, c.socketIO.MakeConnectMsg(namespace)); err != nil {
		log.Println(err)

		signalConnDown(c.connDownCh)
		go c.connectNamespace(namespace)
	}
}

func (c *Client) ConnectToCustomNamespace(namespace string) {
	c.customNamespaces = append(c.customNamespaces, namespace)
	c.connectNamespace(namespace)
}

func (c *Client) OnEvent(namespace, event string, f handler) {
	c.handlers[handlerName{namespace, event}] = f
	c.connectNamespace(namespace)
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

// TODO
//func (c *Client) OnConnect(namespace string, data interface{}) {
//
//}

//func (c *Client) OnError(namespace string, data interface{}) {
//
//}

//func (c *Client) OnDisconnect(namespace string, data interface{}) {
//
//}
