package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func SimulateClient(wg *sync.WaitGroup, url string, data string) {
	defer wg.Done()

	time.Sleep(time.Duration(rand.Intn(2500)) * time.Millisecond)

	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Fatalf("Error connecting to url: %v\n", err)
	}
	defer c.Close()

	for {
		resp, err := http.Get(data)
		if err != nil {
			log.Fatalf("Failed to get data: %v\n", err)
		}

		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		var airport struct {
			Retailers []map[string]interface{}
		}

		if err := json.Unmarshal(body, &airport); err != nil {
			log.Fatalf("Failed to parse data: %v\n", err)
		}

		if err := c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("r%d", rand.Intn(len(airport.Retailers))))); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to send message: %v\n", err)
			return
		}

	loop:
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to read message: %v\n", err)
				return
			}

			switch string(msg) {
			case "o":
				if err := c.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("o%d", rand.Intn(3)))); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to send message: %v\n", err)
					return
				}
			case "c", "s", "f":
				break loop
			}
		}
	}
}

func main() {
	var url string
	var data string
	var clients int
	flag.StringVar(&url, "u", "ws://srcdog.com/airport/ws_customer", "websocket url")
	flag.StringVar(&data, "d", "http://srcdog.com/airport/data", "data url")
	flag.IntVar(&clients, "c", 1, "clients")
	flag.Parse()

	if url == "" {
		log.Fatalln("Websocket url (-u) is required")
	}

	if data == "" {
		log.Fatalln("Data url (-d) is required")
	}

	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go SimulateClient(&wg, url, data)
	}

	wg.Wait()
}
