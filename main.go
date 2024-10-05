/*
 * Main listener
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
	"crypto/tls"
	"html/template"
	"log"
	"net"
	"net/http"
	"time"
)

var tmpl *template.Template

func main() {
	var err error

	if err := fetchConfig("cca.scfg"); err != nil {
		log.Fatal(err)
	}

	log.Println("Setting up database")
	if err := setupDatabase(); err != nil {
		log.Fatal(err)
	}

	log.Println("Setting up JWKS")
	if err := setupJwks(); err != nil {
		log.Fatal(err)
	}

	log.Println("Setting up templates")
	tmpl, err = template.ParseGlob(config.Tmpl)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Setting up courses")
	err = setupCourses()
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Setting up context cancellation connection pool")
	err = setupCancelPool()
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Registering static handle")
	fs := http.FileServer(http.Dir(config.Static))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	log.Println("Registering handlers")
	http.HandleFunc("/{$}", handleIndex)
	http.HandleFunc("/auth", handleAuth)
	http.HandleFunc("/ws", handleWs)

	var l net.Listener

	switch config.Listen.Trans {
	case "plain":
		log.Printf(
			"Establishing plain listener for net \"%s\", addr \"%s\"\n",
			config.Listen.Net,
			config.Listen.Addr,
		)
		l, err = net.Listen(config.Listen.Net, config.Listen.Addr)
		if err != nil {
			log.Fatalf("Failed to establish plain listener: %v\n", err)
		}
	case "tls":
		cer, err := tls.LoadX509KeyPair(config.Listen.TLS.Cert, config.Listen.TLS.Key)
		if err != nil {
			log.Fatalf("Failed to load TLS certificate and key: %v\n", err)
		}
		tlsconfig := &tls.Config{
			Certificates: []tls.Certificate{cer},
			MinVersion:   tls.VersionTLS13,
		} //exhaustruct:ignore
		log.Printf(
			"Establishing TLS listener for net \"%s\", addr \"%s\"\n",
			config.Listen.Net,
			config.Listen.Addr,
		)
		l, err = tls.Listen(config.Listen.Net, config.Listen.Addr, tlsconfig)
		if err != nil {
			log.Fatalf("Failed to establish TLS listener: %v\n", err)
		}
	default:
		log.Fatalln("listen.trans must be \"plain\" or \"tls\"")
	}

	if config.Listen.Proto == "http" {
		log.Println("Serving http")
		srv := &http.Server{
			ReadHeaderTimeout: time.Duration(config.Perf.ReadHeaderTimeout) * time.Second,
		} //exhaustruct:ignore
		err = srv.Serve(l)
	} else {
		log.Fatalln("Unsupported protocol")
	}
	if err != nil {
		log.Fatal(err)
	}
}
