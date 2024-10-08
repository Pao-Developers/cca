/*
 * WebSocket message handlers
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func messageHello(
	ctx context.Context,
	c *websocket.Conn,
	reportError reportErrorT,
	mar []string,
	userID string,
	session string,
) error {
	_, _ = mar, session

	select {
	case <-ctx.Done():
		return fmt.Errorf(
			"%w: %w",
			errContextCancelled,
			ctx.Err(),
		)
	default:
	}

	rows, err := db.Query(
		ctx,
		"SELECT courseid FROM choices WHERE userid = $1",
		userID,
	)
	if err != nil {
		return reportError("error fetching choices")
	}
	courseIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return reportError("error collecting choices")
	}

	if atomic.LoadUint32(&state) == 2 {
		err = writeText(ctx, c, "START")
		if err != nil {
			return fmt.Errorf("%w: %w", errCannotSend, err)
		}
	}
	err = writeText(ctx, c, "HI :"+strings.Join(courseIDs, ","))
	if err != nil {
		return fmt.Errorf("%w: %w", errCannotSend, err)
	}

	return nil
}

func messageChooseCourse(
	ctx context.Context,
	c *websocket.Conn,
	reportError reportErrorT,
	mar []string,
	userID string,
	session string,
	userCourseGroups *userCourseGroupsT,
) error {
	_ = session

	if atomic.LoadUint32(&state) != 2 {
		err := writeText(ctx, c, "E :Course selections are not open")
		if err != nil {
			return fmt.Errorf(
				"%w: %w",
				errCannotSend,
				err,
			)
		}
		return nil
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf(
			"%w: %w",
			errContextCancelled,
			ctx.Err(),
		)
	default:
	}

	if len(mar) != 2 {
		return reportError("Invalid number of arguments for Y")
	}
	_courseID, err := strconv.ParseInt(mar[1], 10, strconv.IntSize)
	if err != nil {
		return reportError("Course ID must be an integer")
	}
	courseID := int(_courseID)

	_course, ok := courses.Load(courseID)
	if !ok {
		return reportError("no such course")
	}
	course, ok := _course.(*courseT)
	if !ok {
		panic("courses map has non-\"*courseT\" items")
	}
	if course == nil {
		return reportError("couse is nil")
	}

	if _, ok := (*userCourseGroups)[course.Group]; ok {
		err := writeText(ctx, c, "R "+mar[1]+" :Group conflict")
		if err != nil {
			return fmt.Errorf(
				"%w: %w",
				errCannotSend,
				err,
			)
		}
		return nil
	}

	err = func() (returnedError error) {
		tx, err := db.Begin(ctx)
		if err != nil {
			return reportError(
				"Database error while beginning transaction",
			)
		}
		defer func() {
			err := tx.Rollback(ctx)
			if err != nil && (!errors.Is(err, pgx.ErrTxClosed)) {
				returnedError = reportError(
					"Database error while rolling back transaction in defer block",
				)
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
				err := writeText(ctx, c, "Y "+mar[1])
				if err != nil {
					return fmt.Errorf(
						"error reaffirming course choice: %w",
						err,
					)
				}
				return nil
			}
			return reportError(
				"Database error while inserting course choice",
			)
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
			go propagateSelectedUpdate(course)
			err := tx.Commit(ctx)
			if err != nil {
				err := course.decrementSelectedAndPropagate(ctx, c)
				if err != nil {
					return fmt.Errorf(
						"%w: %w",
						errCannotSend,
						err,
					)
				}
				return reportError(
					"Database error while committing transaction",
				)
			}

			/*
			 * This would race if message handlers could run
			 * concurrently for one connection.
			 */
			(*userCourseGroups)[course.Group] = struct{}{}

			err = writeText(ctx, c, "Y "+mar[1])
			if err != nil {
				return fmt.Errorf(
					"%w: %w",
					errCannotSend,
					err,
				)
			}

			if config.Perf.PropagateImmediate {
				err = sendSelectedUpdate(ctx, c, courseID)
				if err != nil {
					return fmt.Errorf(
						"%w: %w",
						errCannotSend,
						err,
					)
				}
			}
		} else {
			err := tx.Rollback(ctx)
			if err != nil {
				return reportError(
					"Database error while rolling back transaction due to course limit",
				)
			}
			err = writeText(ctx, c, "R "+mar[1]+" :Full")
			if err != nil {
				return fmt.Errorf(
					"%w: %w",
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

func messageUnchooseCourse(
	ctx context.Context,
	c *websocket.Conn,
	reportError reportErrorT,
	mar []string,
	userID string,
	session string,
	userCourseGroups *userCourseGroupsT,
) error {
	_ = session

	if atomic.LoadUint32(&state) != 2 {
		err := writeText(ctx, c, "E :Course selections are not open")
		if err != nil {
			return fmt.Errorf(
				"%w: %w",
				errCannotSend,
				err,
			)
		}
		return nil
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf(
			"%w: %w",
			errContextCancelled,
			ctx.Err(),
		)
	default:
	}

	if len(mar) != 2 {
		return reportError("Invalid number of arguments for N")
	}
	_courseID, err := strconv.ParseInt(mar[1], 10, strconv.IntSize)
	if err != nil {
		return reportError("Course ID must be an integer")
	}
	courseID := int(_courseID)

	_course, ok := courses.Load(courseID)
	if !ok {
		return reportError("no such course")
	}
	course, ok := _course.(*courseT)
	if !ok {
		panic("courses map has non-\"*courseT\" items")
	}
	if course == nil {
		return reportError("couse is nil")
	}

	ct, err := db.Exec(
		ctx,
		"DELETE FROM choices WHERE userid = $1 AND courseid = $2",
		userID,
		courseID,
	)
	if err != nil {
		return reportError(
			"Database error while deleting course choice",
		)
	}

	if ct.RowsAffected() != 0 {
		err := course.decrementSelectedAndPropagate(ctx, c)
		if err != nil {
			return fmt.Errorf(
				"%w: %w",
				errCannotSend,
				err,
			)
		}

		_course, ok := courses.Load(courseID)
		if !ok {
			return reportError("no such course")
		}
		course, ok := _course.(*courseT)
		if !ok {
			panic("courses map has non-\"*courseT\" items")
		}
		if course == nil {
			return reportError("couse is nil")
		}

		if _, ok := (*userCourseGroups)[course.Group]; !ok {
			return reportError("inconsistent user course groups")
		}
		delete(*userCourseGroups, course.Group)
	}

	err = writeText(ctx, c, "N "+mar[1])
	if err != nil {
		return fmt.Errorf(
			"%w: %w",
			errCannotSend,
			err,
		)
	}

	return nil
}
