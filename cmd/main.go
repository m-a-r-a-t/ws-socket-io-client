package main

import (
	"encoding/json"
	"fmt"
	ws_client "github.com/m-a-r-a-t/ws-socket-io-client"
	"time"
)

func main() {
	cfg := ws_client.ClientConfig{
		Url:           "ws://localhost:8000/socket.io/?EIO=3&transport=websocket",
		AutoReconnect: true,
		WriteTimeout:  time.Second * 10,
		//EmitsRepeatOnError: false,
	}

	c := ws_client.Connect(&cfg)

	//c.OnEvent("/", "reply", func(msg []byte) {
	//	fmt.Println("reply:", string(msg))
	//})
	//
	//c.OnEvent("/", "h1", func(msg []byte) {
	//	var arr []int
	//	json.Unmarshal(msg, &arr)
	//	fmt.Println("h1:", arr)
	//})
	//
	//c.OnEvent("/", "h2", func(msg []byte) {
	//	var m = map[string]string{}
	//	json.Unmarshal(msg, &m)
	//	fmt.Println("h2:", m)
	//})

	//c.ConnectToCustomNamespace("/chat")

	//c.Emit("/chat", "msg", "hello")
	//c.Emit("/", "notice", "hello")
	bytes, _ := json.Marshal(map[string]interface{}{
		"id": "fdsfds",
	})
	fmt.Println(bytes, string(bytes))

	b, _ := json.Marshal(string(bytes))

	fmt.Println(b, string(b))

	fmt.Println("'" + string(bytes) + "'")

	for {
		go c.Emit("/", "notice", b)
		time.Sleep(time.Second * 3)
	}

	time.Sleep(time.Hour * 1)

}
