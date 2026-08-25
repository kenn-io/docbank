package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"time"
)

var (
	mode           = "echo"
	networkAddress string
)

type echoResponse struct {
	Arguments   []string `json:"arguments"`
	Environment []string `json:"environment"`
	StdinSHA256 string   `json:"stdin_sha256"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--descendant" {
		if _, err := fmt.Fprintln(os.Stdout, "descendant-ready"); err != nil || os.Stdout.Sync() != nil {
			os.Exit(6)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	switch mode {
	case "echo", "replacement":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		digest := sha256.Sum256(data)
		_ = json.NewEncoder(os.Stdout).Encode(echoResponse{
			Arguments: os.Args[1:], Environment: os.Environ(),
			StdinSHA256: hex.EncodeToString(digest[:]),
		})
	case "network":
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "unix-network":
		connection, err := net.DialTimeout("unix", networkAddress, time.Second)
		if err != nil {
			fmt.Print("denied")
			return
		}
		_ = connection.Close()
		fmt.Print("connected")
	case "host-file":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			fmt.Print("denied")
		} else {
			fmt.Print("exposed")
		}
	case "descendant":
		command := exec.Command("/proc/self/exe", "--descendant")
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Print("spawned")
		_ = os.Stdout.Sync()
		for {
			time.Sleep(time.Hour)
		}
	case "overflow":
		block := make([]byte, 32<<10)
		for {
			if _, err := os.Stdout.Write(block); err != nil {
				return
			}
		}
	case "exit-125":
		os.Exit(125)
	default:
		os.Exit(4)
	}
}
