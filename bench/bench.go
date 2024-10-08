package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var (
	chosenCourseID = flag.Int("c", -1, "course ID (-1 for random)")
	connections    = flag.Int("n", 10000, "number of connections")
)

var (
	errUnexpectedStatusCode = errors.New("unexpected status code")
	courses                 = big.NewInt(13)
	globalLock              sync.RWMutex
)

func w(ctx context.Context, c *websocket.Conn, m string, cid int) error {
	log.Printf("%d <- %s", cid, m)
	err := c.Write(ctx, websocket.MessageText, []byte(m))
	if err != nil {
		return fmt.Errorf("error writing to connection: %w", err)
	}
	return nil
}

func connect(cid int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, r, err := websocket.Dial(ctx, "wss://localhost.runxiyu.org:8080/ws", nil)
	if err != nil {
		panic(err)
	}
	defer func() {
		err := c.CloseNow()
		if err != nil {
			panic(err)
		}
	}()

	if r.StatusCode != http.StatusSwitchingProtocols {
		panic(errUnexpectedStatusCode)
	}

	err = w(ctx, c, "HELLO", cid)
	if err != nil {
		panic(err)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				cancel()
				log.Printf("%d !R %v", cid, r)
			}
		}()
		for {
			_, msg, err := c.Read(ctx)
			if err != nil {
				panic(err)
			}
			log.Printf("%d -> %s", cid, string(msg))
		}
	}()

	globalLock.RLock()
	defer globalLock.RUnlock()

	if *chosenCourseID == -1 {
		courseID, err := rand.Int(rand.Reader, courses)
		if err != nil {
			panic(err)
		}
		err = w(ctx, c, fmt.Sprintf("Y %d", courseID.Int64()+1), cid)
		if err != nil {
			panic(err)
		}
	} else {
		err = w(ctx, c, fmt.Sprintf("Y %d", *chosenCourseID), cid)
		if err != nil {
			panic(err)
		}
	}

	var deadlock sync.Mutex
	deadlock.Lock()
	deadlock.Lock()
}

func main() {
	flag.Parse()

	globalLock.Lock()
	for i := range *connections {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("%d !M %v", i, r)
				}
			}()
			connect(i)
		}()
		time.Sleep(2 * time.Millisecond)
	}
	for i := range 6 {
		time.Sleep(1 * time.Second)
		log.Printf("waiting %d before trigger", 5-i)
	}
	globalLock.Unlock()

	var deadlock sync.Mutex
	deadlock.Lock()
	deadlock.Lock()
}
