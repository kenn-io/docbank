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

	"golang.org/x/sys/unix"
)

var (
	mode           = "echo"
	networkAddress string
	outputName     = "output.bin"
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
	case "file-network":
		status := "denied"
		connection, err := net.DialTimeout("tcp", networkAddress, time.Second)
		if err == nil {
			status = "connected"
			_ = connection.Close()
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
		}
	case "local-ipc":
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			os.Exit(7)
		}
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
		if err := os.WriteFile("/work/"+outputName, []byte("allowed"), 0o600); err != nil {
			os.Exit(5)
		}
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
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		fmt.Print(status)
	case "file-host":
		_, readErr := os.ReadFile(networkAddress)
		writeErr := os.WriteFile(networkAddress, []byte("modified"), 0o600)
		chmodErr := os.Chmod(networkAddress, 0o644)
		changedTime := time.Unix(1, 0)
		timesErr := os.Chtimes(networkAddress, changedTime, changedTime)
		status := "exposed"
		if readErr != nil && writeErr != nil && chmodErr != nil && timesErr != nil {
			status = "denied"
		}
		if err := os.WriteFile("/work/"+outputName, []byte(status), 0o600); err != nil {
			os.Exit(5)
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
	case "file-output":
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-overflow":
		if err := os.WriteFile("/work/"+outputName, make([]byte, 2048), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-descendant-exit":
		command := exec.Command("/proc/self/exe", "--descendant")
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile("/work/"+outputName, []byte("supervised output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "file-exit81-once":
		if _, err := os.Stat("/work/profile/retry-marker"); err != nil {
			if err := os.WriteFile("/work/profile/retry-marker", []byte("retry"), 0o600); err != nil {
				os.Exit(5)
			}
			os.Exit(81)
		}
		if err := os.WriteFile("/work/"+outputName, []byte("retried output"), 0o600); err != nil {
			os.Exit(5)
		}
	case "exit-125":
		os.Exit(125)
	default:
		os.Exit(4)
	}
}
