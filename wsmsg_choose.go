/*
 * Handle the "Y" message for choosing a course
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
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func messageChooseCourse(
	ctx context.Context,
	c *websocket.Conn,
	mar []string,
	userID string,
	userCourseGroups *userCourseGroupsT,
	userCourseTypes *userCourseTypesT,
) error {
	if atomic.LoadUint32(&state) != 2 {
		err := writeText(ctx, c, "E :Course selections are not open")
		if err != nil {
			return wrapError(
				errCannotSend,
				err,
			)
		}
		return nil
	}

	select {
	case <-ctx.Done():
		return wrapError(
			errContextCanceled,
			ctx.Err(),
		)
	default:
	}

	if len(mar) != 2 {
		return errBadNumberOfArguments
	}
	_courseID, err := strconv.ParseInt(mar[1], 10, strconv.IntSize)
	if err != nil {
		return errNoSuchCourse
	}
	courseID := int(_courseID)

	_course, ok := courses.Load(courseID)
	if !ok {
		return errNoSuchCourse
	}
	course, ok := _course.(*courseT)
	if !ok {
		panic("courses map has non-\"*courseT\" items")
	}
	if course == nil {
		return errNoSuchCourse
	}

	if _, ok := (*userCourseGroups)[course.Group]; ok {
		err := writeText(ctx, c, "R "+mar[1]+" :Group conflict")
		if err != nil {
			return wrapError(
				errCannotSend,
				err,
			)
		}
		return nil
	}

	err = func() (returnedError error) {
		tx, err := db.Begin(ctx)
		if err != nil {
			return wrapError(errUnexpectedDBError, err)
		}
		defer func() {
			err := tx.Rollback(ctx)
			if err != nil && (!errors.Is(err, pgx.ErrTxClosed)) {
				returnedError = wrapError(errUnexpectedDBError, err)
				return
			}
		}()

		_, err = tx.Exec(
			ctx,
			"INSERT INTO choices (seltime, userid, courseid) VALUES ($1, $2, $3)",
			time.Now().UnixMicro(),
			userID,
			courseID,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) &&
				pgErr.Code == pgErrUniqueViolation {
				err2 := writeText(ctx, c, "Y "+mar[1])
				if err2 != nil {
					return wrapError(
						err2,
						wrapError(errUnexpectedDBError, err),
					)
				}
				return nil
			}
			return wrapError(errUnexpectedDBError, err)
		}

		ok := func() bool {
			course.SelectedLock.Lock()
			defer course.SelectedLock.Unlock()
			/*
			 * The read here doesn't have to be atomic because the
			 * lock guarantees that no other goroutine is writing to
			 * it.
			 */
			if course.Selected < course.Max {
				atomic.AddUint32(&course.Selected, 1)
				return true
			}
			return false
		}()

		if ok {
			go func() {
				defer func() {
					if e := recover(); e != nil {
						slog.Error("panic", "arg", e)
					}
				}()
				propagateSelectedUpdate(course)
			}()
			err := tx.Commit(ctx)
			if err != nil {
				err := course.decrementSelectedAndPropagate(ctx, c)
				if err != nil {
					return wrapError(
						errCannotSend,
						err,
					)
				}
				return wrapError(errUnexpectedDBError, err)
			}

			/*
			 * This would race if message handlers could run
			 * concurrently for one connection.
			 */
			(*userCourseGroups)[course.Group] = struct{}{}
			(*userCourseTypes)[course.Type]++

			err = writeText(ctx, c, "Y "+mar[1])
			if err != nil {
				return wrapError(
					errCannotSend,
					err,
				)
			}

			if config.Perf.PropagateImmediate {
				err = sendSelectedUpdate(ctx, c, courseID)
				if err != nil {
					return wrapError(
						errCannotSend,
						err,
					)
				}
			}
		} else {
			err := tx.Rollback(ctx)
			if err != nil {
				return wrapError(errUnexpectedDBError, err)
			}
			err = writeText(ctx, c, "R "+mar[1]+" :Full")
			if err != nil {
				return wrapError(
					errCannotSend,
					err,
				)
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}
	return nil
}
