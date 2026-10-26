package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	port := 8787
	if v := os.Getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("Invalid PORT")
		}
		port = n
	}
	host := os.Getenv("HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" { // for container images without curl
		return healthcheck(port)
	}

	key := os.Getenv("RELAY_REGISTRATION_KEY")
	if file := os.Getenv("RELAY_REGISTRATION_KEY_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return errors.New("Cannot read RELAY_REGISTRATION_KEY_FILE")
		}
		key = strings.TrimSpace(string(data))
	}
	relay, err := New(Config{RegistrationKey: key, PublicOrigin: os.Getenv("PUBLIC_ORIGIN")})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return errors.New("Relay could not listen")
	}
	server := &http.Server{
		Handler:           relay,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second, // cleared by the WebSocket upgrade
		IdleTimeout:       5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		relay.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()
	log.Printf("Private Alma relay listening on port %d", port)
	if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthcheck(port int) error {
	client := http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", res.StatusCode)
	}
	return nil
}
