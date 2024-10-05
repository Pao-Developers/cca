/*
 * Additional synchronization routines
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

type usemT struct {
	_ch (chan struct{})
}

func (s *usemT) init() {
	s._ch = make(chan struct{}, 1)
}

func (s *usemT) set() {
	select {
	case s._ch <- struct{}{}:
	default:
	}
}

func (s *usemT) ch() (<- chan struct{}) {
	return s._ch
}
