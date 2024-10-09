/*
 * WebSocket connection routine
 *
 * Copyright (C) 2024  Runxi Yu <https://runxiyu.org>
 * SPDX-License-Identifier: AGPL-3.0-or-later
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type errbytesT struct {
	err   error
	bytes *[]byte
}

var usemCount int64

/*
 * This is more appropriately typed as uint64, but it needs to be cast to int64
 * later anyway due to time.Duration, so let's just use int64.
 */

/*
 * The actual logic in handling the connection, after authentication has been
 * completed.
 */
func handleConn(
	ctx context.Context,
	c *websocket.Conn,
	session string,
	userID string,
) (retErr error) {
	reportError := makeReportError(ctx, c)
	newCtx, newCancel := context.WithCancel(ctx)

	_cancel, ok := cancelPool.Load(userID)
	if ok {
		cancel, ok := _cancel.(*context.CancelFunc)
		if ok && cancel != nil {
			(*cancel)()
		}
		/* TODO: Make the cancel synchronous */
	}
	cancelPool.Store(userID, &newCancel)

	defer func() {
		cancelPool.CompareAndDelete(userID, &newCancel)
		if errors.Is(retErr, context.Canceled) {
			/*
			 * Only works if it's newCtx that has been cancelled
			 * rather than the original ctx, which is kinda what
			 * we intend
			 */
			_ = writeText(ctx, c, "E :Context canceled")
		}
		/* TODO: Report errors properly */
	}()

	/* TODO: Tell the user their current choices here. Deprecate HELLO. */

	usems := make(map[int]*usemT)

	/* TODO: Check if the LoadUint32 here is a bit too much overhead */
	atomic.AddInt64(&usemCount, int64(atomic.LoadUint32(&numCourses)))
	courses.Range(func(key, value interface{}) bool {
		/* TODO: Remember to change this too when changing the courseID type */
		courseID, ok := key.(int)
		if !ok {
			panic("courses map has non-\"int\" keys")
		}
		course, ok := value.(*courseT)
		if !ok {
			panic("courses map has non-\"*courseT\" items")
		}
		usem := &usemT{} //exhaustruct:ignore
		usem.init()
		course.Usems.Store(userID, usem)
		usems[courseID] = usem
		return true
	})

	defer func() {
		courses.Range(func(key, value interface{}) bool {
			_ = key
			course, ok := value.(*courseT)
			if !ok {
				panic("courses map has non-\"*courseT\" items")
			}
			course.Usems.Delete(userID)
			return true
		})
		atomic.AddInt64(&usemCount, -int64(atomic.LoadUint32(&numCourses)))
	}()

	usemParent := make(chan int)
	for courseID, usem := range usems {
		go func() {
			for {
				select {
				case <-newCtx.Done():
					return
				case <-usem.ch:
					select {
					case <-newCtx.Done():
						return
					case usemParent <- courseID:
					}
				}
				time.Sleep(
					time.Duration(
						atomic.LoadInt64(&usemCount)>>
							config.Perf.UsemDelayShiftBits,
					) * time.Millisecond,
				)
			}
		}()
	}

	/*
	 * userCourseGroups stores whether the user has already chosen a course
	 * in the courseGroup.
	 */
	var userCourseGroups userCourseGroupsT = make(map[courseGroupT]struct{})
	err := populateUserCourseGroups(newCtx, &userCourseGroups, userID)
	if err != nil {
		return reportError(
			fmt.Sprintf(
				"cannot populate user course groups: %v",
				err,
			),
		)
	}

	/*
	 * Later we need to select from recv and send and perform the
	 * corresponding action. But we can't just select from c.Read because
	 * the function blocks. Therefore, we must spawn a goroutine that
	 * blocks on c.Read and send what it receives to a channel "recv"; and
	 * then we can select from that channel.
	 */
	recv := make(chan *errbytesT)
	go func() {
		for {
			/*
			 * Here we use the original connection context instead
			 * of the new context we just created. Apparently when
			 * the context passed to Read expires, the connection
			 * gets closed, which makes it impossible for us to
			 * write the context expiry message to the client.
			 * So we pass the original connection context, which
			 * would get cancelled anyway once we close the
			 * connection.
			 * See: https://github.com/coder/websocket/issues/242
			 * We still need to take care of this while sending so
			 * we don't infinitely block, and leak goroutines and
			 * cause the channel to remain out of reach of the
			 * garbage collector.
			 * It would be nice to return the newCtx.Err() but
			 * the only way to really do that is to use the recv
			 * channel which might not have a listener anymore.
			 * It's not really crucial anyways so we could just
			 * close this goroutine by returning here.
			 */
			_, b, err := c.Read(ctx)
			if err != nil {
				/*
				 * TODO: Prioritize context dones... except
				 * that it's not really possible. I would just
				 * have placed newCtx in here but apparently
				 * that causes the connection to be closed when
				 * the context expires, which makes it
				 * impossible to deliver the final error
				 * message. Probably need to look into this
				 * design again.
				 */
				select {
				case <-newCtx.Done():
					_ = writeText(
						ctx,
						c,
						"E :Context canceled",
					)
					/* Not a typo to use ctx here */
					return
				case recv <- &errbytesT{err: err, bytes: nil}:
				}
				return
			}
			select {
			case <-newCtx.Done():
				_ = writeText(ctx, c, "E :Context cancelled")
				/* Not a typo to use ctx here */
				return
			case recv <- &errbytesT{err: nil, bytes: &b}:
			}
		}
	}()

	for {
		var mar []string
		select {
		case <-newCtx.Done():
			/*
			 * TODO: Somehow prioritize this case over all other
			 * cases
			 */
			return fmt.Errorf(
				"%w: %w",
				errContextCancelled,
				newCtx.Err(),
			)
			/*
			 * There are other times when the context could be
			 * cancelled, and apparently some WebSocket functions
			 * just close the connection when the context is
			 * cancelled. So it's kinda impossible to reliably
			 * send this message due to newCtx cancellation.
			 * But in any case, the WebSocket connection would
			 * be closed, and the user would see the connection
			 * closed page which should explain it.
			 */
		case courseID := <-usemParent:
			err := sendSelectedUpdate(newCtx, c, courseID)
			if err != nil {
				return fmt.Errorf(
					"%w: %w",
					errCannotSend,
					err,
				)
			}
			continue
		case errbytes := <-recv:
			if errbytes.err != nil {
				return fmt.Errorf(
					"%w: %w",
					errCannotReceiveMessage,
					errbytes.err,
				)
				/*
				 * Note that this cannot return newCtx.Err(),
				 * so we handle the error reporting in the
				 * reading routine
				 */
			}
			mar = splitMsg(errbytes.bytes)
			switch mar[0] {
			case "HELLO":
				err := messageHello(
					newCtx,
					c,
					reportError,
					mar,
					userID,
					session,
				)
				if err != nil {
					return err
				}
			case "Y":
				err := messageChooseCourse(
					newCtx,
					c,
					reportError,
					mar,
					userID,
					session,
					&userCourseGroups,
				)
				if err != nil {
					return err
				}
			case "N":
				err := messageUnchooseCourse(
					newCtx,
					c,
					reportError,
					mar,
					userID,
					session,
					&userCourseGroups,
				)
				if err != nil {
					return err
				}
			default:
				return reportError("Unknown command " + mar[0])
			}
		}
	}
}

var cancelPool sync.Map /* string, *context.CancelFunc */
