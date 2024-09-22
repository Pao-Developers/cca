/*
 * Primary WebSocket routines
 *
 * Copyright (C) 2024  Runxi Yu <https://runxiyu.org>
 * SPDX-License-Identifier: BSD-2-Clause
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *     1. Redistributions of source code must retain the above copyright
 *     notice, this list of conditions and the following disclaimer.
 *
 *     2. Redistributions in binary form must reproduce the above copyright
 *     notice, this list of conditions and the following disclaimer in the
 *     documentation and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS "AS IS" AND ANY
 * EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR
 * PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR
 * CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL,
 * EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO,
 * PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR
 * PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF
 * LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING
 * NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
 * SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 */

/*
 * The message format is a WebSocket message separated with spaces.
 * The contents of each field could contain anything other than spaces,
 * null bytes, carriage returns, and newlines. The first character of
 * each argument cannot be a colon. As an exception, the last argument may
 * contain spaces and the first character thereof may be a colon, if the
 * argument is prefixed with a colon. The colon used for the prefix is not
 * considered part of the content of the message. For example, in
 *
 *    SQUISH POP :cat purr!!
 *
 * the first field is "SQUISH", the second field is "POP", and the third
 * field is "cat purr!!".
 *
 * It is essentially an RFC 1459 IRC message without trailing CR-LF and
 * without prefixes. See section 2.3.1 of RFC 1459 for an approximate
 * BNF representation.
 *
 * The reason this was chosen instead of using protobuf etc. is that it
 * is simple to parse without external libraries, and it also happens to
 * be a format I'm very familiar with, having extensively worked with the
 * IRC protocol.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
)

/*
 * Handle requests to the WebSocket endpoint and establish a connection.
 * Authentication is handled here, but afterwards, the connection is really
 * handled in handleConn.
 */
func handleWs(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		Subprotocols: []string{"cca1"},
	})
	if err != nil {
		w.Write([]byte("This endpoint only supports valid WebSocket connections."))
		return
	}
	defer c.CloseNow()

	/*
	 * TODO: Here we fetch the cookie from the HTTP headers. On browser's
	 * I've tested, creating WebSocket connections with JavaScript still
	 * passes httponly cookies in the upgrade request. I'm not sure if this
	 * is true for all browsers and it wasn't simple to find a spec for
	 * this.
	 */
	sessionCookie, err := req.Cookie("session")
	if errors.Is(err, http.ErrNoCookie) {
		c.Write(
			req.Context(),
			websocket.MessageText,
			[]byte("U"),
		)
		return
	} else if err != nil {
		c.Write(
			req.Context(),
			websocket.MessageText,
			[]byte("E :Error fetching cookie"),
		)
		return
	}

	var userid string
	var expr int

	err = db.QueryRow(
		context.Background(),
		"SELECT userid, expr FROM sessions WHERE cookie = $1",
		sessionCookie.Value,
	).Scan(&userid, &expr)
	if errors.Is(err, pgx.ErrNoRows) {
		c.Write(
			req.Context(),
			websocket.MessageText,
			[]byte("U"), /* Unauthenticated */
		)
	} else if err != nil {
		c.Write(
			req.Context(),
			websocket.MessageText,
			[]byte("E :Database error"),
		)
	}

	/*
	 * Now that we have an authenticated request, this WebSocket connection
	 * may be simply associated with the session and userid.
	 * TODO: There are various race conditions that could occur if one user
	 * creates multiple connections, with the same or different session
	 * cookies. The last situation could occur in normal use when a user
	 * opens multiple instances of the page in one browser, and is not
	 * unique to custom clients or malicious users. Some effort must be
	 * taken to ensure that each user may only have one connection at a
	 * time.
	 */
	err = handleConn(
		req.Context(),
		c,
		sessionCookie.Value,
		userid,
	)
	if err != nil {
		log.Printf("%v", err)
		return
	}
}

/*
 * Split an IRC-style message of type []byte into type []string where each
 * element is a complete argument. Generally, arguments are separated by
 * spaces, and an argument that begins with a ':' causes the rest of the
 * line to be treated as a single argument.
 */
func splitMsg(b *[]byte) []string {
	mar := make([]string, 0, 4)
	elem := make([]byte, 0, 5)
	for i, c := range *b {
		switch c {
		case ' ':
			if (*b)[i+1] == ':' {
				mar = append(mar, string(elem))
				mar = append(mar, string((*b)[i+2:]))
				goto endl
			}
			mar = append(mar, string(elem))
			elem = make([]byte, 0, 5)
		default:
			elem = append(elem, c)
		}
	}
	mar = append(mar, string(elem))
endl:
	return mar
}

type errbytes_t struct {
	err   error
	bytes *[]byte
}

var chanPool [](chan string)

func setupChanPool() error {
	chanPool = make([](chan string), 0)
	return nil
}

/*
 * The actual logic in handling the connection, after authentication has been
 * completed.
 */
func handleConn(
	ctx context.Context,
	c *websocket.Conn,
	session string,
	userid string,
) error {
	/*
	 * TODO: Remember to delete the "send" channel from "chanPool", so that
	 * it could be garbage collected, and so the broadcast routine won't
	 * send to it. This should be done in a "defer" statement following the
	 * append. It is not presently clear how this should be implemented, as
	 * there is no simple way to remove an element from a slice. We could
	 * create a new slice with the contents of the existing slice but that
	 * would be pretty heavy; we could also set elements to nil (and
	 * perhaps reuse nil slots in the future) while being sure to use
	 * atomic indexes (and perhaps using a map instead of a slice); we
	 * could also handle chanPool in a separate goroutine.
	 */
	send := make(chan string)
	chanPool = append(chanPool, send)

	/*
	 * Later we need to select from recv and send and perform the
	 * corresponding action. But we can't just select from c.Read because
	 * the function blocks. Therefore, we must spawn a goroutine that
	 * blocks on c.Read and send what it receives to a channel "recv"; and
	 * then we can select from that channel.
	 */
	recv := make(chan *errbytes_t)
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				recv <- &errbytes_t{err: err, bytes: nil}
				return
			}
			recv <- &errbytes_t{err: nil, bytes: &b}
		}
	}()

	for {
		var mar []string
		select {
		case gonnasend := <-send:
			err := c.Write(ctx, websocket.MessageText, []byte(gonnasend))
			if err != nil {
				return err
			}
			continue
		case errbytes := <-recv:
			if (*errbytes).err != nil {
				return (*errbytes).err
			}
			mar = splitMsg((*errbytes).bytes)
			switch mar[0] {
			case "HELLO":
				c.Write(ctx, websocket.MessageText, []byte("HI"))
			default:
				c.Write(
					ctx,
					websocket.MessageText,
					[]byte(fmt.Sprintf("E :Unknown command %s", mar[0])),
				)
			}
		}
	}
}
