package main

import (
	"context"
	"encoding/json"
	"fmt"
	ws_client "github.com/m-a-r-a-t/ws-socket-io-client"
	"log"
	"log/slog"
	"time"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	cfg := ws_client.ClientConfig{
		Url:           "ws://localhost:8999/socket.io/?EIO=3&transport=websocket",
		AutoReconnect: true,
		WriteTimeout:  time.Second * 10,
	}

	c, err := ws_client.NewClient(&cfg)
	if err != nil {
		log.Fatal(err)
	}

	c.OnConnect(func(msg []byte) {
		slog.Info("CONNECT", slog.Any("msg", msg))
	})

	c.OnEventSync("/", "counter", func(msg []byte) {
		var r Resp
		json.Unmarshal(msg, &r)

		fmt.Println(r.Counter)
	})

	bytes, _ := json.Marshal("hello world")
	//b, _ := json.Marshal(string(bytes))

	ctx := context.Background()

	err = c.Connect()
	if err != nil {
		log.Fatal(err)
	}

	for {
		_, err := c.EmitWithAck(ctx, "/", "hello", bytes)
		if err != nil {
			slog.Error("can not emit msg", slog.Any("err", err))
		}

		//time.Sleep(time.Second)

		//json.Unmarshal()

		//fmt.Println(string(data))
	}

	time.Sleep(time.Hour * 1)
}

type Resp struct {
	Counter int `json:"counter"`
}
